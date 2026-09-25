package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// This host-only call is never accepted by decodeActionToolRequest. It shares
// the existing CRM subprocess and authentication, without another model tool.
func (e *Engine) caseArtifactCall(ctx context.Context, token string, p ActionPrincipal, request map[string]any) (map[string]any, error) {
	ctx, p, err := e.caseToolContext(ctx, token, p, json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	e.actionMu.RLock()
	host := actionHostForCommand(e.actionHost, "case-read")
	e.actionMu.RUnlock()
	tools, ok := host.(ActionToolHost)
	if !ok {
		return nil, errors.New("case host unavailable")
	}
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	result, card, err := tools.Tool(ctx, "case-artifact", input, p, token, e.i18n.CurrentLang())
	if card != nil {
		return nil, errors.New("unexpected case approval")
	}
	return result, err
}

func (e *Engine) prepareCaseDelivery(ctx context.Context, host FileWorkHost, token string, p ActionPrincipal, workID string, input json.RawMessage) (string, error) {
	e.actionMu.RLock()
	enabled := e.sharedCasesEnabled
	e.actionMu.RUnlock()
	if !enabled {
		return "", nil
	}
	var request struct {
		Path            string `json:"path"`
		SHA256          string `json:"sha256"`
		ExpectedVersion int    `json:"expected_version"`
	}
	if err := json.Unmarshal(input, &request); err != nil {
		return "", err
	}
	data, _ := json.Marshal([]any{workID, request.Path, request.SHA256, request.ExpectedVersion})
	digest := sha256.Sum256(data)
	key := hex.EncodeToString(digest[:])
	work, err := host.Tool(ctx, "work-context", json.RawMessage(`{}`), p, workID)
	if err != nil {
		return "", err
	}
	data, _ = json.Marshal(work)
	var current FileWorkContext
	if err := json.Unmarshal(data, &current); err != nil {
		return "", err
	}
	existing := false
	for _, artifact := range current.Artifacts {
		if artifact.Path == request.Path && artifact.SHA256 == request.SHA256 && artifact.Version == request.ExpectedVersion+1 {
			existing = true
		}
	}
	result, err := e.caseArtifactCall(ctx, token, p, map[string]any{"phase": "prepare", "request_key": key, "existing_delivery": existing})
	if err != nil {
		return "", err
	}
	if result["status"] == "disabled" || result["status"] == "not_shared" {
		return "", nil
	}
	if result["status"] != "ready" {
		return "", errors.New("case delivery preparation unavailable")
	}
	return key, nil
}

// CaseSource is captured before alias/quote enrichment, then saved with the
// accepted FileTurn. A resumed turn may use only these original source grants.
type CaseSource struct {
	MessageID     string `json:"message_id"`
	Text          string `json:"source_text"`
	OriginalText  string `json:"original_text,omitempty"`
	ShareAllowed  bool   `json:"share_allowed"`
	SourceReceipt string `json:"source_receipt,omitempty"`
	SelectedCase  string `json:"selected_case,omitempty"`
}

type CaseContext struct {
	WorkID            string            `json:"work_id"`
	SourceText        string            `json:"source_text"`
	OriginalText      string            `json:"original_text,omitempty"`
	ShareAllowed      bool              `json:"share_allowed"`
	SourceReceipt     string            `json:"source_receipt,omitempty"`
	SelectedCase      string            `json:"selected_case,omitempty"`
	OfferConfirmation bool              `json:"offer_confirmation,omitempty"`
	Confirmation      *CaseAnswer       `json:"confirmation,omitempty"`
	session           *Session          `json:"-"`
	state             *interactiveState `json:"-"`
}

type CaseSelection struct {
	Name      string          `json:"name"`
	Principal ActionPrincipal `json:"principal"`
}

func (e *Engine) rememberCase(ctx context.Context, p ActionPrincipal, result map[string]any) {
	binding, ok := TrustedCaseContext(ctx)
	if !ok || binding.WorkID == "" || binding.session == nil || binding.state == nil {
		result["session_binding"] = "unavailable"
		return
	}
	data, _ := json.Marshal(result["case"])
	var record struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &record) != nil || record.Name == "" || len([]rune(record.Name)) > 120 {
		return
	}
	binding.state.mu.Lock()
	stale := binding.state.stopped || binding.state.fileWorkID != binding.WorkID || binding.state.fileSession != binding.session || !sameFilePrincipal(binding.state.actionPrincipal, p)
	binding.state.mu.Unlock()
	if stale {
		result["session_binding"] = "unavailable"
		return
	}
	sm := e.sessions
	sm.mu.Lock()
	defer sm.mu.Unlock()
	// Pinned when the file-work process started, including ordinary first turns
	// which have no pending FileTurn. Never follow the active cursor after RPC.
	s := binding.session
	if sm.sessions[s.ID] != s || !slices.Contains(sm.userSessions[p.SessionKey], s.ID) || sm.loadErr != nil || sm.storePath == "" {
		result["session_binding"] = "unavailable"
		return
	}
	s.mu.Lock()
	previous := s.CaseSelection
	if previous != nil && (previous.Name != record.Name || !sameFilePrincipal(previous.Principal, p)) {
		s.mu.Unlock()
		result["session_binding"] = "conflict"
		return
	}
	s.CaseSelection = &CaseSelection{Name: record.Name, Principal: p}
	s.mu.Unlock()
	if sm.saveLockedError() != nil {
		s.mu.Lock()
		s.CaseSelection = previous
		s.mu.Unlock()
		result["session_binding"] = "unavailable"
	}
}

type caseContextKey struct{}

func (e *Engine) importCaseArtifacts(ctx context.Context, token string, p ActionPrincipal, result map[string]any) {
	data, err := json.Marshal(result["case"])
	if err != nil {
		return
	}
	var record struct {
		Artifacts []struct {
			Receipt string `json:"message_receipt"`
			Stale   bool   `json:"stale"`
		} `json:"artifacts"`
	}
	if json.Unmarshal(data, &record) != nil {
		return
	}
	host, workID := e.fileWorkForToken(token)
	if host == nil {
		return
	}
	// ponytail: at most four recent artifacts per read, matching bounded intake.
	// Older files stay listed with their source revision; never claim all revised.
	count := 0
	for _, artifact := range record.Artifacts {
		if count >= 4 {
			break
		}
		input, _ := json.Marshal(map[string]string{"receipt": artifact.Receipt})
		work, err := host.Tool(ctx, "host-case-import", input, p, workID)
		if err != nil || work == nil || work["status"] == "blocked" {
			result["file_import"] = map[string]any{"status": "unavailable", "code": "case_receipt_import_unavailable"}
			return
		}
		result["work_context"] = work
		count++
	}
	result["file_import"] = map[string]any{"status": "available", "imported": count, "remaining": len(record.Artifacts) - count}
}

// TrustedCaseContext is available only on a host-authenticated tool invocation.
func TrustedCaseContext(ctx context.Context) (CaseContext, bool) {
	v, ok := ctx.Value(caseContextKey{}).(CaseContext)
	return v, ok
}

func isCaseCommand(command string) bool {
	return command == "case-list" || command == "case-read" || command == "case-update" || command == "case-member-add"
}

// AuthorizeProjectFile is a host-only read. File intake has no model token yet;
// the adapter authenticates this call with its existing protected host secret.
// The file host supplies the transport principal and destination work, never a
// model path, claimed role or arbitrary recipient. Empty chat means legacy mode.
func (e *Engine) AuthorizeProjectFile(ctx context.Context, p ActionPrincipal, workID, receipt string, requireBinding bool) (string, error) {
	e.actionMu.RLock()
	host := actionHostForCommand(e.actionHost, "case-read")
	enabled := e.sharedCasesEnabled
	e.actionMu.RUnlock()
	tools, ok := host.(ActionToolHost)
	if !enabled || !ok || (requireBinding && workID == "") {
		return "", errors.New("project file authorization unavailable")
	}
	if workID == "" {
		workID = "host-reference-check"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, caseContextKey{}, CaseContext{WorkID: workID})
	input, _ := json.Marshal(map[string]any{"receipt": receipt, "require_binding": requireBinding})
	// The CLI requires a 32-character context marker even for read-only host
	// calls. This is not a model session token; host-secret authentication remains.
	result, card, err := tools.Tool(ctx, "case-access", input, p, "host-project-file-access-not-a-model-token", e.i18n.CurrentLang())
	if err != nil {
		slog.Warn("project file authorization host call failed", "error_type", fmt.Sprintf("%T", err))
		return "", fmt.Errorf("project file authorization host: %w", err)
	}
	if card != nil {
		return "", errors.New("project file authorization unavailable")
	}
	if result["status"] == "legacy" {
		return "", nil
	}
	chat, _ := result["source_chat_id"].(string)
	if result["status"] != "authorized" || chat == "" {
		if result["status"] == "blocked" || result["status"] == "disabled" {
			return "", ErrFileWorkNotFound
		}
		return "", errors.New("project file authorization unavailable")
	}
	return chat, nil
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
	source := CaseSource{MessageID: msg.MessageID, Text: text, OriginalText: text, ShareAllowed: !msg.FileWorkPrivate}
	if msg.ParentMessageID == "" && fileConversationIntent(text) != "new" {
		if s := e.sessions.FindByID(e.sessions.ActiveSessionID(msg.SessionKey)); s != nil {
			s.mu.Lock()
			if selection := s.CaseSelection; selection != nil && sameFilePrincipal(selection.Principal, e.actionPrincipalForMessage(msg)) {
				source.SelectedCase = selection.Name
			}
			s.mu.Unlock()
		}
	}
	for _, denial := range []string{"不要共享", "不要同步", "不要记入", "别共享", "别同步", "别记入", "不许同步", "不能同步", "不要告诉", "别告诉", "保密", "仅私下"} {
		if strings.Contains(text, denial) {
			source.ShareAllowed = false
			msg.caseSources = []CaseSource{source}
			return
		}
	}
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
	confirmations := e.caseConfirmationsEnabled
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
					binding := CaseContext{WorkID: state.fileWorkID, SourceText: source.Text, OriginalText: source.OriginalText, ShareAllowed: source.ShareAllowed, SourceReceipt: source.SourceReceipt, SelectedCase: source.SelectedCase, session: state.fileSession, state: state}
					binding.OfferConfirmation = confirmations
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
