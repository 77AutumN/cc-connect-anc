package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// FileWorkBinding is assembled only from the authenticated transport and host
// filesystem configuration. None of these fields are model tool arguments.
type FileWorkBinding struct {
	Principal     ActionPrincipal
	SessionID     string
	Route         json.RawMessage
	WorkRoot      string
	OwnerUID      int
	Inputs        []FileAttachment
	DeferInputs   bool   // Keep busy-turn attachments in host-only snapshots until dequeue.
	Group         bool   // Authenticated transport chat type, never a model argument.
	SourceReceipt string // Explicit group reply to one delivered artifact.
}

type FileWorkInput struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Path   string          `json:"path"`
	SHA256 string          `json:"sha256"`
	Source *FileWorkSource `json:"source,omitempty"`
}

type FileWorkSource struct {
	WorkID         string `json:"work_id"`
	DeliveryID     string `json:"delivery_id"`
	Version        int    `json:"version"`
	MessageReceipt string `json:"message_receipt"`
}

type FileWorkArtifact struct {
	Path           string `json:"path"`
	DeliveryID     string `json:"delivery_id"`
	Version        int    `json:"version"`
	Status         string `json:"status"`
	Name           string `json:"name"`
	SHA256         string `json:"sha256"`
	MessageReceipt string `json:"message_receipt,omitempty"`
}

type FileWorkContext struct {
	Enabled        bool               `json:"enabled"`
	WorkID         string             `json:"work_id"`
	WorkRoot       string             `json:"-"`
	Inputs         []FileWorkInput    `json:"inputs"`
	OutputDir      string             `json:"output_dir"`
	LatestVersion  int                `json:"latest_version"`
	Artifacts      []FileWorkArtifact `json:"artifacts"`
	IncomingInputs []FileWorkInput    `json:"-"` // Host intake receipt, never model-visible pending materials.
}

type FileWorkRef struct {
	WorkID, SessionID string
	Import            bool // Copy a group artifact into the requesting actor's own work.
}

type FileWorkHost interface {
	Bind(context.Context, FileWorkBinding) (FileWorkContext, error)
	FindByMessage(context.Context, ActionPrincipal, string) (FileWorkRef, error)
	RecordReply(context.Context, ActionPrincipal, string) error
	ActivateInputs(context.Context, ActionPrincipal, string) (FileWorkContext, error)
	Tool(context.Context, string, json.RawMessage, ActionPrincipal, string) (map[string]any, error)
}

// FileWorkReceiver opts a transport into bounded intake. Reply authorization
// happens before downloading an unmentioned group attachment.
type FileWorkReceiver interface {
	SetFileWorkEnabled(bool, func(Message, string) error) error
	SetFileWorkReplyObserver(func(Message, string) error)
	FileWorkReplyContext(json.RawMessage) (any, error)
}

// FileWorkAgent creates an isolated process configuration for exactly one work.
// Unsupported runtimes must fail before launching an ordinary agent process.
type FileWorkAgent interface {
	ForFileWork(string) (Agent, error)
}

func isFileWorkCommand(command string) bool {
	return command == "work-context" || command == "file-deliver" || command == "file-status"
}

var errFileWorkUnavailable = errors.New("controlled file work unavailable")
var ErrFileWorkNotFound = errors.New("file work not found")
var ErrFileWorkNeedsArtifact = errors.New("group file receipt required")
var ErrFileWorkUnconfirmed = errors.New("file_delivery_requires_reconciliation")

// Call only for a rejected intake, never an accepted, queued or started turn.
func logFileWorkRejected(msg *Message, reason MsgKey) {
	if msg.ControlledFileWork {
		slog.Info("file work intake rejected", "msg_id", msg.MessageID,
			"platform", msg.Platform, "session", msg.SessionKey, "reason", reason)
	}
}

// SetFileWorkHost is called only during startup, after protected configuration
// and runtime support have been checked. A nil host leaves legacy behavior off.
func (e *Engine) SetFileWorkHost(host FileWorkHost, prepare func(string) (string, int, error)) error {
	if host == nil || prepare == nil {
		return errFileWorkUnavailable
	}
	if _, ok := e.agent.(FileWorkAgent); !ok {
		return errFileWorkUnavailable
	}
	if e.multiWorkspace {
		return errFileWorkUnavailable
	}
	for _, p := range e.platforms {
		receiver, ok := p.(FileWorkReceiver)
		if !ok {
			return errFileWorkUnavailable
		}
		if _, ok := p.(FileReceiptSender); !ok {
			return errFileWorkUnavailable
		}
		if err := receiver.SetFileWorkEnabled(true, func(msg Message, parent string) error {
			ref, err := host.FindByMessage(e.ctx, e.actionPrincipalForMessage(&msg), parent)
			if msg.FileWorkPrivate && (ref.Import || errors.Is(err, ErrFileWorkNeedsArtifact) || errors.Is(err, ErrFileWorkUnconfirmed)) {
				return ErrFileWorkNotFound
			}
			return err
		}); err != nil {
			return err
		}
		receiver.SetFileWorkReplyObserver(func(msg Message, receipt string) error {
			return host.RecordReply(e.ctx, e.actionPrincipalForMessage(&msg), receipt)
		})
	}
	e.actionMu.Lock()
	e.fileWorkHost, e.fileWorkPrepare = host, prepare
	e.actionMu.Unlock()
	return nil
}

// Private messages continue the sender's active work. Explicit replies take
// precedence; groups still require an owned reply to continue an earlier work.
func (e *Engine) handleFileWorkMessage(p Platform, msg *Message) bool {
	e.actionMu.RLock()
	host, prepare := e.fileWorkHost, e.fileWorkPrepare
	e.actionMu.RUnlock()
	if host == nil && !msg.ControlledFileWork {
		return false
	}
	reject := func(reason MsgKey, reply string) bool {
		e.reply(p, msg.ReplyCtx, reply)
		// This intake did not dispatch or enqueue a model turn. A reply receipt
		// binding failure is a separate outbound event, not a completion signal.
		logFileWorkRejected(msg, reason)
		return true
	}
	fail := func(key MsgKey) bool { return reject(key, e.i18n.T(key)) }
	if host == nil || prepare == nil || !msg.ControlledFileWork {
		return fail(MsgFileWorkUnavailable)
	}
	e.sessions.mu.RLock()
	storageError := e.sessions.loadErr
	e.sessions.mu.RUnlock()
	if storageError != nil {
		return fail(MsgFileSupplementSaveFailed)
	}
	sender, ok := p.(FileReceiptSender)
	if !ok {
		return fail(MsgFileWorkUnavailable)
	}
	route, err := sender.FileReplyRoute(msg.ReplyCtx)
	if err != nil {
		return fail(MsgFileWorkUnavailable)
	}
	principal := e.actionPrincipalForMessage(msg)
	if principal.UserID == "" || principal.ChatID == "" || principal.MessageID == "" {
		return fail(MsgFileWorkUnavailable)
	}
	// ponytail: one intake lock per engine; split by principal only if intake
	// throughput requires it. File processing itself runs outside this lock.
	e.fileWorkMu.Lock()
	defer e.fileWorkMu.Unlock()
	intent := ""
	if msg.FileWorkPrivate && len(msg.Files) == 0 {
		intent = fileConversationIntent(msg.Content)
	}
	if intent == "stop" {
		e.cmdStop(p, msg)
		return true
	}
	var session *Session
	var busy *Session
	for _, existing := range e.sessions.ListSessions(msg.SessionKey) {
		if existing.Busy() {
			busy = existing
		}
	}
	if busy != nil && msg.ParentMessageID == "" && (!msg.FileWorkPrivate || intent == "new") {
		return fail(MsgPreviousProcessing)
	}
	lookup := msg.ParentMessageID
	// A selected upload is material, not a work reference. The instruction's
	// own ID recovers its durable binding if transport redelivers after restart.
	if lookup == "" || msg.FileWorkNewInput {
		lookup = msg.MessageID
	}
	ref, lookupErr := host.FindByMessage(e.ctx, principal, lookup)
	if errors.Is(lookupErr, ErrFileWorkUnconfirmed) {
		return fail(MsgFileRecoveryUnknown)
	}
	sourceReceipt := ""
	if lookupErr == nil && ref.Import {
		if msg.FileWorkPrivate || msg.ParentMessageID == "" {
			return fail(MsgFileWorkAssociationRequired)
		}
		if busy != nil {
			return fail(MsgPreviousProcessing)
		}
		sourceReceipt = msg.ParentMessageID
		session = e.sessions.NewSession(msg.SessionKey, "file work")
	} else if lookupErr == nil {
		for _, existing := range e.sessions.ListSessions(msg.SessionKey) {
			if fileNativeSessionID(existing) == ref.SessionID {
				session = existing
				break
			}
		}
		if session == nil {
			return fail(MsgFileWorkAssociationRequired)
		}
	} else if (msg.ParentMessageID == "" || (msg.FileWorkNewInput && !msg.FileWorkPrivate && len(msg.Files) == 1)) && errors.Is(lookupErr, ErrFileWorkNotFound) {
		if busy != nil && msg.FileWorkNewInput {
			return fail(MsgPreviousProcessing)
		}
		if msg.FileWorkPrivate && intent != "new" {
			session = e.sessions.GetOrCreateActive(msg.SessionKey)
		} else {
			session = e.sessions.NewSession(msg.SessionKey, "file work")
		}
	} else {
		return fail(MsgFileWorkAssociationRequired)
	}
	if busy != nil && busy != session {
		return fail(MsgPreviousProcessing)
	}
	for _, turn := range session.fileTurns() {
		if turn.Principal.MessageID == msg.MessageID || turn.ResumeMessageID == msg.MessageID {
			runMessageAccepted(msg)
			return true
		}
	}
	root, uid, err := prepare(fileNativeSessionID(session))
	if err != nil {
		return fail(MsgFileWorkUnavailable)
	}
	work, err := host.Bind(e.ctx, FileWorkBinding{Principal: principal, SessionID: fileNativeSessionID(session), Route: route, WorkRoot: root, OwnerUID: uid, Inputs: msg.Files, DeferInputs: busy != nil || session.hasUnfinishedFileTurns(), Group: !msg.FileWorkPrivate, SourceReceipt: sourceReceipt})
	if err != nil {
		if message, ok := FileErrorMessage(err, e.i18n); ok {
			return reject(MsgFileInputSaveFailed, message)
		}
		return fail(MsgFileInputSaveFailed)
	}
	if !work.Enabled || work.WorkID == "" || work.WorkRoot != root {
		return fail(MsgFileWorkUnavailable)
	}
	// Originals are durable in the host before acknowledging a supplement.
	if sourceReceipt != "" {
		for i := range msg.caseSources {
			msg.caseSources[i].SourceReceipt = sourceReceipt
		}
	}
	msg.fileSession = session
	msg.fileTurn = &FileTurn{Principal: principal, WorkID: work.WorkID, Content: msg.Content, Route: route, Status: "queued", CaseSources: append([]CaseSource(nil), msg.caseSources...)}
	inputCount := len(msg.Files)
	if sourceReceipt != "" {
		inputCount++
	}
	if inputCount > 0 {
		if len(work.IncomingInputs) != inputCount {
			return fail(MsgFileInputSaveFailed)
		}
		msg.fileTurn.Inputs = append([]FileWorkInput(nil), work.IncomingInputs...)
		msg.Content += "\n[Host: selected files were preserved for this work. Read work-context and the registered input paths before answering.]"
		if sourceReceipt != "" {
			msg.Content += "\n[Host: this group reply continues the one delivered file recorded in input.source. Use its preserved input snapshot; the original actor's conversation and other files are not shared. Produce a new revision without claiming a shared conversation or global version.]"
		}
		msg.fileTurn.Content = msg.Content
	}
	msg.Files = nil
	if busy == nil && session.hasUnfinishedFileTurns() {
		if e.prepareFileRecovery(p, msg, session, work) {
			return true
		}
	}
	agent, err := e.agent.(FileWorkAgent).ForFileWork(root)
	if err != nil {
		return fail(MsgFileWorkUnavailable)
	}
	e.interactiveMu.Lock()
	old := e.interactiveStates[msg.SessionKey]
	different := false
	if old != nil {
		old.mu.Lock()
		different = old.fileWorkID != work.WorkID
		old.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	if different {
		e.cleanupInteractiveState(msg.SessionKey, old)
	}
	if _, err := e.sessions.SwitchSession(msg.SessionKey, session.ID); err != nil {
		return fail(MsgFileWorkAssociationRequired)
	}
	if !session.TryLock() {
		if e.queueMessageForBusySession(p, msg, msg.SessionKey) {
			if session.TryLock() {
				go e.drainOrphanedQueue(session, e.sessions, msg.SessionKey, agent, "")
			}
			return true
		}
		return fail(MsgPreviousProcessing)
	}
	// The running turn may finish while Bind saves attachments. This is still
	// an accepted supplement, even when it can now start without waiting.
	if busy != nil {
		if err := e.sessions.addFileTurn(session, *msg.fileTurn, e.maxQueuedMessages); err != nil {
			session.UnlockWithoutUpdate()
			return fail(MsgFileSupplementSaveFailed)
		}
	}
	e.ensureInteractiveStateForQueueing(msg.SessionKey, p, msg.ReplyCtx)
	e.interactiveMu.Lock()
	state := e.interactiveStates[msg.SessionKey]
	state.mu.Lock()
	state.fileWorkID = work.WorkID
	state.mu.Unlock()
	e.interactiveMu.Unlock()
	session.TouchUserActivity()
	runMessageAccepted(msg)
	go e.processInteractiveMessageWith(p, msg, session, agent, e.sessions, msg.SessionKey, "", msg.SessionKey)
	return true
}

// Only an explicit opening clause in the current user's own text is a control
// instruction. Ambiguous topic changes remain conversational clarification.
func fileConversationIntent(content string) string {
	text := strings.TrimSpace(content)
	switch strings.TrimRight(text, "。.!！ ") {
	case "先停下", "停一下", "停止当前工作", "先暂停", "stop", "Stop":
		return "stop"
	}
	clause := strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune("，,:：。.!！\n", r)
	})
	if len(clause) > 0 {
		switch strings.TrimSpace(clause[0]) {
		case "换个事", "换一件事", "另开一件事", "另外做一件事", "新任务", "开始一项新工作", "new task", "New task":
			return "new"
		}
	}
	return ""
}

func fileNativeSessionID(session *Session) string {
	// A reset/lost SessionManager counter must not silently reopen an older work.
	return fmt.Sprintf("%s@%d", session.ID, session.CreatedAt.UnixNano())
}

func (e *Engine) fileWorkForToken(token string) (FileWorkHost, string) {
	e.actionMu.RLock()
	host := e.fileWorkHost
	e.actionMu.RUnlock()
	if host == nil {
		return nil, ""
	}
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for _, state := range e.interactiveStates {
		state.mu.Lock()
		matches := state.actionToken == token && !state.stopped
		id := state.fileWorkID
		state.mu.Unlock()
		if matches && id != "" {
			return host, id
		}
	}
	return nil, ""
}

func (e *Engine) serveFileWorkTool(w http.ResponseWriter, r *http.Request, token string, principal ActionPrincipal, command string, input json.RawMessage) {
	host, workID := e.fileWorkForToken(token)
	if host == nil {
		writeActionToolResult(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "enabled": false, "code": "file_host_disabled"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	caseKey := ""
	if command == "file-deliver" {
		var err error
		caseKey, err = e.prepareCaseDelivery(ctx, host, token, principal, workID, input)
		if err != nil {
			writeActionToolResult(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "code": "case_delivery_prepare_failed"})
			return
		}
	}
	result, err := host.Tool(ctx, command, input, principal, workID)
	if result != nil && result["status"] == "blocked" {
		writeActionToolResult(w, http.StatusBadRequest, result)
		return
	}
	if err != nil || result == nil {
		// The failure may be a post-send journal write. Never imply that the
		// recipient saw nothing or invite resubmission under another filename.
		writeActionToolResult(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "code": "file_outcome_unconfirmed"})
		return
	}
	if caseKey != "" && result["status"] == "accepted" {
		registration, registrationErr := e.caseArtifactCall(ctx, token, principal, map[string]any{"phase": "finish", "request_key": caseKey, "artifact": result})
		if registrationErr != nil || registration == nil {
			registration = map[string]any{"status": "unavailable", "code": "case_registration_unconfirmed_do_not_resend"}
		}
		result["case_registration"] = registration
	}
	writeActionToolResult(w, http.StatusOK, result)
}
