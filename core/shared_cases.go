package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// CaseSource is captured before alias/quote enrichment, then saved with the
// accepted FileTurn. A resumed turn may use only these original source grants.
type CaseSource struct {
	MessageID    string `json:"message_id"`
	Text         string `json:"source_text"`
	ShareAllowed bool   `json:"share_allowed"`
}

type CaseContext struct {
	WorkID       string `json:"work_id"`
	SourceText   string `json:"source_text"`
	ShareAllowed bool   `json:"share_allowed"`
}

type caseContextKey struct{}

// TrustedCaseContext is available only on a host-authenticated tool invocation.
func TrustedCaseContext(ctx context.Context) (CaseContext, bool) {
	v, ok := ctx.Value(caseContextKey{}).(CaseContext)
	return v, ok
}

func isCaseCommand(command string) bool {
	return command == "case-list" || command == "case-read" || command == "case-update"
}

func (e *Engine) SetSharedCasesEnabled(enabled bool) {
	e.actionMu.Lock()
	defer e.actionMu.Unlock()
	e.sharedCasesEnabled = enabled
}

func (e *Engine) captureCaseSource(msg *Message) {
	msg.caseSources = nil
	e.actionMu.RLock()
	enabled := e.sharedCasesEnabled
	e.actionMu.RUnlock()
	if !enabled || !msg.ControlledFileWork || msg.IsPermissionResponse || msg.InteractionRequestID != "" {
		return
	}
	text := strings.TrimSpace(msg.Content)
	if len([]rune(text)) > 16000 || msg.MessageID == "" {
		return
	}
	source := CaseSource{MessageID: msg.MessageID, Text: text, ShareAllowed: !msg.FileWorkPrivate}
	if msg.FileWorkPrivate {
		// ponytail: one explicit sharing paragraph, not general intent inference.
		// Ambiguous requests remain private; Claude asks for the selected content.
		for _, prefix := range []string{"同步给执行", "同步给销售", "记入这场婚宴", "把以下内容同步给执行", "把以下内容记入这场婚宴"} {
			body := strings.TrimPrefix(text, "请")
			if !strings.HasPrefix(body, prefix) {
				continue
			}
			body = strings.TrimSpace(strings.TrimPrefix(body, prefix))
			if !strings.HasPrefix(body, "：") && !strings.HasPrefix(body, ":") && !strings.HasPrefix(body, "，") {
				continue
			}
			body = strings.TrimSpace(strings.TrimLeft(body, "：:,，"))
			body, _, _ = strings.Cut(body, "\n")
			if body != "" && !strings.Contains(body, "不要同步") && !strings.Contains(body, "不要共享") {
				source.Text, source.ShareAllowed = body, true
			}
			break
		}
	}
	msg.caseSources = []CaseSource{source}
}

// Match both the token and current principal again under the state lock. A
// queued turn cannot lend its permission to an in-flight earlier invocation.
func (e *Engine) caseToolContext(ctx context.Context, token string, p ActionPrincipal, input json.RawMessage) (context.Context, ActionPrincipal, error) {
	e.actionMu.RLock()
	enabled := e.sharedCasesEnabled
	e.actionMu.RUnlock()
	if !enabled {
		return ctx, p, errors.New("shared cases disabled")
	}
	var request struct {
		SourceMessageID string `json:"source_message_id"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return ctx, p, err
	}
	if request.SourceMessageID == "" {
		request.SourceMessageID = p.MessageID
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		if state.actionToken == token && state.currentPrincipal == p && !state.stopped && state.fileWorkID != "" {
			for _, source := range state.caseSources {
				if source.MessageID == request.SourceMessageID {
					binding := CaseContext{WorkID: state.fileWorkID, SourceText: source.Text, ShareAllowed: source.ShareAllowed}
					p.MessageID = source.MessageID
					state.mu.Unlock()
					return context.WithValue(ctx, caseContextKey{}, binding), p, nil
				}
			}
		}
		state.mu.Unlock()
	}
	return ctx, p, errors.New("case source unavailable")
}
