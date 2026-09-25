package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// One bounded question per existing conversation; not a task/approval queue.
// Instances are immutable after assignment so SessionManager can snapshot them.
type CaseQuestion struct {
	ID        string          `json:"id"`
	Command   string          `json:"command"`
	Input     json.RawMessage `json:"input"`
	Principal ActionPrincipal `json:"principal"`
	Context   CaseContext     `json:"context"`
	Question  string          `json:"question"`
	Revision  int             `json:"revision"`
	Status    string          `json:"status"`
	Receipt   string          `json:"receipt,omitempty"`
	AnswerID  string          `json:"answer_id,omitempty"`
	AskedAtMs int64           `json:"asked_at_ms"`
	Expires   time.Time       `json:"expires"`
	Private   bool            `json:"private"`
	recovered bool
}

type CaseAnswer struct {
	Command         string          `json:"command"`
	Input           json.RawMessage `json:"input"`
	AnswerMessageID string          `json:"answer_message_id"`
	Revision        int             `json:"revision"`
}

func (e *Engine) SetCaseConfirmationsEnabled(enabled bool) {
	e.actionMu.Lock()
	defer e.actionMu.Unlock()
	e.caseConfirmationsEnabled = enabled
}

func (sm *SessionManager) saveCaseQuestion(s *Session, q *CaseQuestion) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.sessions[s.ID] != s || sm.storePath == "" || sm.loadErr != nil {
		return errors.New("case question storage unavailable")
	}
	s.mu.Lock()
	previous := s.CaseQuestion
	previousAnswers := s.CaseAnswerIDs
	previousSelection := s.CaseSelection
	if q.Status == "executing" && !slices.Contains(s.CaseAnswerIDs, q.AnswerID) {
		s.CaseAnswerIDs = append(append([]string(nil), s.CaseAnswerIDs...), q.AnswerID)
		if len(s.CaseAnswerIDs) > retainedFileTurns {
			s.CaseAnswerIDs = s.CaseAnswerIDs[len(s.CaseAnswerIDs)-retainedFileTurns:]
		}
	}
	s.CaseQuestion = q
	if q.Status == "done" {
		var request struct {
			CaseName string `json:"case_name"`
		}
		if json.Unmarshal(q.Input, &request) == nil && request.CaseName != "" {
			s.CaseSelection = &CaseSelection{Name: request.CaseName, Principal: q.Principal}
		}
	}
	s.mu.Unlock()
	if err := sm.saveLockedError(); err != nil {
		s.mu.Lock()
		s.CaseQuestion = previous
		s.CaseAnswerIDs = previousAnswers
		s.CaseSelection = previousSelection
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Session) caseQuestion() *CaseQuestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CaseQuestion
}

func (e *Engine) askCaseQuestion(ctx context.Context, command string, input json.RawMessage, principal ActionPrincipal, platform Platform, reply any, result map[string]any) (map[string]any, error) {
	binding, ok := TrustedCaseContext(ctx)
	if !ok || !binding.OfferConfirmation || binding.session == nil || binding.state == nil || (command != "case-read" && command != "case-update" && command != "case-member-add") {
		return nil, errors.New("case clarification unavailable")
	}
	var preview struct {
		Question string
		Revision int
	}
	raw, err := json.Marshal(result)
	if err != nil || json.Unmarshal(raw, &preview) != nil || preview.Question == "" || len([]rune(preview.Question)) > 3500 || preview.Revision < 0 {
		return nil, errors.New("invalid business question")
	}
	var request struct {
		CaseName string `json:"case_name"`
	}
	if json.Unmarshal(input, &request) != nil || request.CaseName == "" {
		return map[string]any{"status": "needs_selection", "code": "explicit_case_candidate_required"}, nil
	}
	// ponytail: serialize confirmation transitions per test entry. No background
	// worker; split this lock only if rollout beyond the two accounts needs it.
	e.caseConfirmationMu.Lock()
	defer e.caseConfirmationMu.Unlock()
	binding.state.mu.Lock()
	private := binding.state.filePrivate
	valid := !binding.state.stopped && binding.state.fileSession == binding.session && binding.state.fileWorkID == binding.WorkID && sameFilePrincipal(binding.state.currentPrincipal, principal) && binding.state.pending == nil
	binding.state.mu.Unlock()
	if !valid || e.sessions.ActiveSessionID(principal.SessionKey) != binding.session.ID {
		return nil, errors.New("case question work changed")
	}
	if old := binding.session.caseQuestion(); old != nil && (old.Status == "executing" || (old.Status == "awaiting" && time.Now().Before(old.Expires))) {
		return map[string]any{"status": "awaiting_confirmation", "code": "answer_current_business_question_first"}, nil
	}
	id, err := newActionToken()
	if err != nil {
		return nil, err
	}
	q := &CaseQuestion{ID: "case:" + id, Command: command, Input: append(json.RawMessage(nil), input...), Principal: principal,
		Context: binding, Question: preview.Question, Revision: preview.Revision, Status: "preparing", Expires: time.Now().Add(24 * time.Hour)}
	q.Private = private
	q.Context.session, q.Context.state, q.Context.Confirmation = nil, nil, nil
	if err := e.sessions.saveCaseQuestion(binding.session, q); err != nil {
		return nil, err
	}
	if err := e.publishCaseQuestion(ctx, platform, reply, binding.session, q); err != nil {
		return nil, err
	}
	return map[string]any{"status": "awaiting_confirmation", "instruction": "The host asked the concrete business question. Wait for its result; do not ask a second question or claim execution."}, nil
}

func (e *Engine) publishCaseQuestion(ctx context.Context, p Platform, reply any, s *Session, original *CaseQuestion) error {
	publisher, ok := p.(HostedActionCardPublisher)
	refresher, refreshOK := p.(CardMessageRefresher)
	if !ok || !refreshOK {
		return errors.New("business question transport unavailable")
	}
	q := *original
	placeholder := NewCard().Title(e.i18n.T(MsgAskQuestionTitle), "blue").PlainText(q.Question).Build()
	if err := e.waitOutgoing(p); err != nil {
		return err
	}
	q.AskedAtMs = time.Now().UnixMilli()
	receipt, err := publisher.ReplyHostedActionPlaceholder(ctx, reply, placeholder)
	if err != nil || receipt == "" {
		return errors.New("business question delivery unconfirmed")
	}
	q.Receipt, q.Status, q.recovered = receipt, "awaiting", false
	if err := e.sessions.saveCaseQuestion(s, &q); err != nil {
		return err
	}
	buttons := []CardButton{PrimaryBtn(e.i18n.T(MsgDeleteModeConfirmButton), "askq:0:1"), DefaultBtn(e.i18n.T(MsgCRMCancelButton), "askq:0:2")}
	for i := range buttons {
		buttons[i].Extra = map[string]string{"askq_label": buttons[i].Text, "askq_question": q.Question}
	}
	card := NewCard().Title(e.i18n.T(MsgAskQuestionTitle), "blue").PlainText(q.Question).Buttons(buttons...).Build()
	card.Interaction = &CardInteraction{RequestID: q.ID + ":0", Principal: q.Principal}
	if err := refresher.RefreshCardMessage(ctx, receipt, q.Principal.SessionKey, card); err != nil {
		q.Status = "delivery_unknown"
		_ = e.sessions.saveCaseQuestion(s, &q)
		return errors.New("business question delivery unconfirmed")
	}
	return nil
}

func caseAnswerValue(text string, callback bool) int {
	if callback {
		switch text {
		case "askq:0:1":
			return 1
		case "askq:0:2":
			return -1
		}
		return 0
	}
	switch strings.ToLower(strings.Trim(strings.TrimSpace(text), "。.!！")) {
	case "确认", "是", "是的", "对", "对的", "嗯", "嗯嗯", "好", "好的", "可以", "就这样", "yes", "confirm":
		return 1
	case "取消", "不是", "不对", "不要", "先别", "no", "cancel":
		return -1
	}
	return 0
}

func (e *Engine) callCaseQuestion(ctx context.Context, q *CaseQuestion, resultOnly bool) (map[string]any, error) {
	e.actionMu.RLock()
	host := actionHostForCommand(e.actionHost, q.Command)
	enabled := e.sharedCasesEnabled && e.caseConfirmationsEnabled
	e.actionMu.RUnlock()
	tools, ok := host.(ActionToolHost)
	if !ok || !enabled {
		return nil, errors.New("case host disabled")
	}
	binding := q.Context
	binding.OfferConfirmation = false
	command, input := q.Command, q.Input
	if resultOnly {
		command = "case-result"
		input, _ = json.Marshal(map[string]any{"command": q.Command, "input": q.Input})
		binding.Confirmation = nil
	} else {
		binding.Confirmation = &CaseAnswer{Command: q.Command, Input: q.Input, AnswerMessageID: q.AnswerID, Revision: q.Revision}
	}
	ctx = context.WithValue(ctx, caseContextKey{}, binding)
	result, card, err := tools.Tool(ctx, command, input, q.Principal, "host-case-question-not-a-model-token", e.i18n.CurrentLang())
	if card != nil {
		return nil, errors.New("unexpected case approval")
	}
	return result, err
}

// Handles only the current host-owned question, before generic AskUserQuestion.
// Returning false after success feeds a scoped result through the existing work
// queue, so Claude continues without treating "yes" as a new sharing grant.
func (e *Engine) handleCaseAnswer(p Platform, msg *Message, content string) bool {
	callback := strings.HasPrefix(msg.InteractionRequestID, "case:")
	e.actionMu.RLock()
	enabled := e.sharedCasesEnabled && e.caseConfirmationsEnabled
	e.actionMu.RUnlock()
	if !enabled {
		return callback
	}
	e.caseConfirmationMu.Lock()
	defer e.caseConfirmationMu.Unlock()
	s := e.sessions.FindByID(e.sessions.ActiveSessionID(msg.SessionKey))
	if s == nil {
		return callback
	}
	previous := s.caseQuestion()
	if previous == nil {
		return callback
	}
	q := *previous
	s.mu.Lock()
	duplicate := msg.MessageID != "" && slices.Contains(s.CaseAnswerIDs, msg.MessageID)
	s.mu.Unlock()
	if duplicate && q.Status == "awaiting" {
		return true
	}
	if !sameFilePrincipal(q.Principal, e.actionPrincipalForMessage(msg)) {
		return callback
	}
	if callback && msg.InteractionRequestID != q.ID+":0" {
		return true
	}
	if q.Status != "awaiting" && q.Status != "executing" {
		return callback
	}
	// An unquoted text answer must be newer than this concrete question.
	// A transport without a timestamp can still bind an explicit reply/card.
	if !callback && msg.ParentMessageID != q.Receipt && (q.AskedAtMs == 0 || msg.UserMessageTimeMs <= q.AskedAtMs) {
		if caseAnswerValue(content, false) != 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionRetry))
			return true
		}
		return false
	}
	key := e.interactiveKeyForSessionKey(msg.SessionKey)
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if state != nil && q.Status == "awaiting" {
		state.mu.Lock()
		competing := state.pending != nil
		state.mu.Unlock()
		if competing {
			q.Status = "cancelled"
			if e.sessions.saveCaseQuestion(s, &q) != nil {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFileSupplementSaveFailed))
				return true
			}
			return callback
		}
	}
	intent := fileConversationIntent(content)
	if intent != "" || strings.HasPrefix(content, "/") || (msg.ParentMessageID != "" && msg.ParentMessageID != q.Receipt) {
		if q.Status == "awaiting" || q.Status == "preparing" {
			q.Status = "cancelled"
			if err := e.sessions.saveCaseQuestion(s, &q); err != nil {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFileSupplementSaveFailed))
				return true
			}
		}
		return callback
	}
	answer := caseAnswerValue(content, callback)
	if (q.recovered || q.Status == "executing") && (content == "继续" || content == "继续处理") {
		answer = 1
	}
	if answer == 0 || len(msg.Files) > 0 || len(msg.Images) > 0 || (msg.ExtraContent != "" && msg.ParentMessageID != q.Receipt) {
		// A new substantive message is not an answer to this question. Expire
		// it before another model question can make a later "yes" ambiguous.
		if !callback && q.Status == "awaiting" {
			q.Status = "cancelled"
			if e.sessions.saveCaseQuestion(s, &q) != nil {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFileSupplementSaveFailed))
				return true
			}
		}
		return callback
	}
	if q.Status != "awaiting" && q.Status != "executing" {
		return callback
	}
	if time.Now().After(q.Expires) && q.Status != "executing" {
		q.Status = "expired"
		_ = e.sessions.saveCaseQuestion(s, &q)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionRetry))
		return true
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	if answer < 0 && q.Status != "executing" {
		q.Status = "cancelled"
		if e.sessions.saveCaseQuestion(s, &q) != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFileSupplementSaveFailed))
			return true
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSessionCancelled))
		return true
	}
	if q.recovered || q.Status == "executing" {
		result, err := e.callCaseQuestion(ctx, &q, true)
		if err != nil || result == nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionUnknown))
			return true
		}
		if result["status"] == "recorded" || result["status"] == "found" {
			return e.finishCaseQuestion(p, msg, content, s, &q, answer > 0)
		}
		if result["status"] != "not_executed" {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionRetry))
			return true
		}
		if answer < 0 {
			q.Status = "cancelled"
			if e.sessions.saveCaseQuestion(s, &q) != nil {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionUnknown))
				return true
			}
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgSessionCancelled))
			return true
		}
		// A restart invalidates the old question even if its button was clicked.
		if revision, ok := result["current_revision"].(float64); ok && revision != float64(q.Revision) {
			q.Status = "needs_review"
			_ = e.sessions.saveCaseQuestion(s, &q)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionRetry))
			return true
		}
		id, err := newActionToken()
		if err != nil {
			return true
		}
		q.ID, q.Status, q.Receipt, q.AnswerID = "case:"+id, "preparing", "", ""
		q.Expires = time.Now().Add(24 * time.Hour)
		if e.sessions.saveCaseQuestion(s, &q) != nil || e.publishCaseQuestion(ctx, p, msg.ReplyCtx, s, &q) != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionUnknown))
		}
		return true
	}
	q.AnswerID = msg.MessageID
	if callback {
		q.AnswerID = q.ID
	} // Authenticated transport question binding.
	if q.AnswerID == "" {
		return true
	}
	q.Status = "executing"
	if e.sessions.saveCaseQuestion(s, &q) != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgFileSupplementSaveFailed))
		return true
	}
	result, err := e.callCaseQuestion(ctx, &q, false)
	if err != nil || result == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionUnknown))
		return true
	}
	if result["status"] != "recorded" && result["status"] != "found" {
		q.Status = "needs_review"
		if e.sessions.saveCaseQuestion(s, &q) != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionUnknown))
			return true
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionRetry))
		return true
	}
	return e.finishCaseQuestion(p, msg, content, s, &q, true)
}

func (e *Engine) finishCaseQuestion(p Platform, msg *Message, content string, s *Session, q *CaseQuestion, resume bool) bool {
	q.Status = "done"
	if e.sessions.saveCaseQuestion(s, q) != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionUnknown))
		return true
	}
	// No model assertion creates this selection; the host operation just checked
	// membership, work binding and the frozen revision again.
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCaseQuestionDone))
	if !resume {
		return true
	}
	var request struct {
		CaseName string `json:"case_name"`
	}
	_ = json.Unmarshal(q.Input, &request)
	msg.MessageID = q.AnswerID
	if msg.MessageID == "" {
		msg.MessageID = q.ID
	}
	msg.ControlledFileWork, msg.FileWorkPrivate, msg.ChannelID = true, q.Private, q.Principal.ChatID
	msg.InteractionRequestID, msg.IsPermissionResponse = "", false
	msg.ParentMessageID = q.Principal.MessageID
	msg.Content = fmt.Sprintf("[Host-confirmed business result; not a new authorization]\n%s\n%s", q.Question, e.i18n.T(MsgCaseQuestionDone))
	msg.caseSources = []CaseSource{{MessageID: msg.MessageID, Text: content, OriginalText: content, SelectedCase: request.CaseName}}
	msg.caseAnswerContinuation = true
	return false
}
