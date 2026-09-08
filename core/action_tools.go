package core

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ActionToolHost is optional. Its model surface has no caller-supplied identity
// or approval operation. The adapter validates each command's business fields.
type ActionToolHost interface {
	Tool(context.Context, string, json.RawMessage, ActionPrincipal, string, Language) (map[string]any, *ActionHostResult, error)
}

// ActionToolHandler is an opt-in handler, NOT installed on APIServer. A caller
// must serve it on a separate local transport, never expose the privileged
// cron/relay socket to an agent. No listener or service is started here.
func (e *Engine) ActionToolHandler() http.Handler {
	return ActionToolsHandler(e)
}

// ActionToolsHandler serves fixed environments on one local listener. Tokens
// are matched against live host-owned sessions; ambiguity always fails closed.
func ActionToolsHandler(engines ...*Engine) http.Handler {
	engines = append([]*Engine(nil), engines...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/tool" || r.URL.RawQuery != "" {
			writeActionToolResult(w, http.StatusNotFound, map[string]any{"status": "blocked", "code": "unsupported_route"})
			return
		}
		authorization := r.Header.Values("Authorization")
		if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") {
			writeActionToolResult(w, http.StatusUnauthorized, map[string]any{"status": "blocked", "code": "invalid_session"})
			return
		}
		token := strings.TrimPrefix(authorization[0], "Bearer ")
		var e *Engine
		var principal ActionPrincipal
		var platform Platform
		var replyCtx any
		for _, candidate := range engines {
			if candidate == nil {
				continue
			}
			p, transport, reply, valid := candidate.actionToolSession(token)
			if !valid {
				continue
			}
			if e != nil {
				writeActionToolResult(w, http.StatusUnauthorized, map[string]any{"status": "blocked", "code": "invalid_session"})
				return
			}
			e, principal, platform, replyCtx = candidate, p, transport, reply
		}
		if e == nil {
			writeActionToolResult(w, http.StatusUnauthorized, map[string]any{"status": "blocked", "code": "invalid_session"})
			return
		}
		command, input, err := decodeActionToolRequest(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			writeActionToolResult(w, http.StatusBadRequest, map[string]any{"status": "blocked", "code": "invalid_input"})
			return
		}
		e.actionMu.RLock()
		host := e.actionHost
		e.actionMu.RUnlock()
		tools, ok := host.(ActionToolHost)
		if !ok {
			writeActionToolResult(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "code": "tool_host_disabled"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		stop := context.AfterFunc(e.ctx, cancel)
		defer stop()
		if ctx.Err() != nil || e.ctx.Err() != nil {
			writeActionToolResult(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "code": "request_cancelled"})
			return
		}
		result, approval, err := tools.Tool(ctx, command, input, principal, token, e.i18n.CurrentLang())
		if err == nil && approval != nil {
			err = e.publishHostedActionContext(ctx, host, *approval, principal, platform, replyCtx)
		}
		if err != nil || result == nil {
			// Keep the persisted reference queryable, but do not leak subprocess
			// output, credentials, or imply that the approval card was delivered.
			unavailable := map[string]any{"status": "unavailable", "code": "tool_or_delivery_failed"}
			if id, ok := result["change_id"].(string); ok {
				unavailable["change_id"] = id
			}
			if approval != nil && approval.ChangeID != "" {
				unavailable["change_id"] = approval.ChangeID
			}
			writeActionToolResult(w, http.StatusServiceUnavailable, unavailable)
			return
		}
		writeActionToolResult(w, http.StatusOK, result)
	})
}

// Snapshot under existing state locks, then release them before backend or
// network calls. The original principal is pinned for this process/token:
// shared-channel reuse must not turn an old child token into a new user's key.
func (e *Engine) actionToolSession(token string) (ActionPrincipal, Platform, any, bool) {
	if token == "" || len(token) > 1024 || e.ctx.Err() != nil {
		return ActionPrincipal{}, nil, nil, false
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		if state.actionToken == "" || subtle.ConstantTimeCompare([]byte(state.actionToken), []byte(token)) != 1 {
			state.mu.Unlock()
			continue
		}
		p, original := state.currentPrincipal, state.actionPrincipal
		p.MessageID, original.MessageID = "", ""
		valid := !state.stopped && state.agentSession != nil && state.agentSession.Alive() &&
			state.platform != nil && p == original && p.Platform == state.platform.Name() && p.Project == e.name &&
			p.UserID != "" && p.ChatID != "" && p.SessionKey != ""
		principal, platform, replyCtx := state.currentPrincipal, state.platform, state.replyCtx
		state.mu.Unlock()
		return principal, platform, replyCtx, valid
	}
	return ActionPrincipal{}, nil, nil, false
}

func decodeActionToolRequest(r io.Reader) (string, json.RawMessage, error) {
	d := json.NewDecoder(r)
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return "", nil, errors.New("invalid tool envelope")
	}
	fields := make(map[string]json.RawMessage, 2)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return "", nil, err
		}
		name, ok := key.(string)
		if !ok || (name != "command" && name != "input") || fields[name] != nil {
			return "", nil, errors.New("unexpected tool field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return "", nil, err
		}
		fields[name] = value
	}
	if _, err := d.Token(); err != nil {
		return "", nil, err
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		return "", nil, errors.New("trailing tool data")
	}
	var command string
	if err := json.Unmarshal(fields["command"], &command); err != nil {
		return "", nil, err
	}
	if command != "customer" && command != "stage" && command != "result" && command != "assignee" && command != "stage-customer-create" && command != "stage-customer-update" {
		return "", nil, errors.New("unsupported tool")
	}
	input := bytes.TrimSpace(fields["input"])
	if len(input) == 0 || input[0] != '{' {
		return "", nil, errors.New("input must be an object")
	}
	return command, input, nil
}

func writeActionToolResult(w http.ResponseWriter, status int, result map[string]any) {
	data, err := json.Marshal(result)
	if err != nil {
		status = http.StatusServiceUnavailable
		data = []byte(`{"status":"unavailable","code":"invalid_tool_result"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
