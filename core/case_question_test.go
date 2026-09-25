package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func caseQuestionFixture(t *testing.T) (*Engine, *actionToolHostStub, *hostedCardPlatform, *interactiveState, *Session) {
	t.Helper()
	e, h, p, state := actionToolFixture(t)
	t.Cleanup(e.cancel)
	e.sessions = NewSessionManager(filepath.Join(t.TempDir(), "sessions.json"))
	s := e.sessions.GetOrCreateActive(state.currentPrincipal.SessionKey)
	state.fileSession, state.fileWorkID = s, "fixture-work"
	state.filePrivate = true
	state.caseSources = []CaseSource{{MessageID: state.currentPrincipal.MessageID, Text: "再加一桌，同步这个变更", OriginalText: "再加一桌，同步这个变更"}}
	e.SetSharedCasesEnabled(true)
	e.SetCaseConfirmationsEnabled(true)
	h.toolResult = map[string]any{"status": "needs_confirmation", "question": "将林陈婚宴改为23桌并同步给项目成员，确认吗？", "revision": 1}
	return e, h, p, state, s
}

const questionToolRequest = `{"command":"case-update","input":{"case_name":"林陈婚宴","expected_revision":1,"changes":[{"field":"桌数","value":"23桌","state":"proposed","quote":"再加一桌","replaces":true}]}}`

func askFixtureCaseQuestion(t *testing.T, e *Engine, state *interactiveState, s *Session) *CaseQuestion {
	t.Helper()
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, questionToolRequest)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "awaiting_confirmation") {
		t.Fatal(w.Code, w.Body.String())
	}
	q := s.caseQuestion()
	if q == nil || q.Status != "awaiting" || q.Receipt == "" {
		t.Fatal(q)
	}
	p := state.platform.(*hostedCardPlatform)
	for _, element := range p.refreshes[len(p.refreshes)-1].Elements {
		if actions, ok := element.(CardActions); ok {
			for _, button := range actions.Buttons {
				if button.Extra["askq_label"] != button.Text || button.Extra["askq_question"] != q.Question {
					t.Fatal("business answer card would show internal callback text")
				}
			}
		}
	}
	return q
}

func answerFixture(q *CaseQuestion, text string, callback bool) *Message {
	m := &Message{Platform: q.Principal.Platform, UserID: q.Principal.UserID, ChannelID: q.Principal.ChatID, SessionKey: q.Principal.SessionKey,
		MessageID: "answer-message", Content: text, ControlledFileWork: true, FileWorkPrivate: true, UserMessageTimeMs: time.Now().UnixMilli() + 1}
	if callback {
		m.InteractionRequestID = q.ID + ":0"
		m.MessageID = ""
	}
	return m
}

func TestCaseQuestionJourneyPersistsExecutesAndRejectsRepeatedAnswer(t *testing.T) {
	e, h, p, state, s := caseQuestionFixture(t)
	q := askFixtureCaseQuestion(t, e, state, s)
	loaded := NewSessionManager(e.sessions.storePath).FindByID(s.ID).caseQuestion()
	if loaded == nil {
		t.Fatal("question not saved")
	}
	var savedInput, input any
	_ = json.Unmarshal(loaded.Input, &savedInput)
	_ = json.Unmarshal(q.Input, &input)
	if !loaded.recovered || loaded.Context.ShareAllowed || !reflect.DeepEqual(savedInput, input) {
		t.Fatal(loaded)
	}
	h.toolResult = map[string]any{"status": "recorded", "case": map[string]any{"name": "林陈婚宴", "revision": 2}}
	m := answerFixture(q, "askq:0:1", true)
	m.ControlledFileWork, m.FileWorkPrivate, m.ChannelID = false, false, "" // Real transport callbacks omit file intake fields.
	if e.handleCaseAnswer(p, m, m.Content) || !m.caseAnswerContinuation || h.toolCalls != 2 {
		t.Fatal("did not continue original work", h.toolCalls, m)
	}
	if m.ParentMessageID != q.Principal.MessageID || m.caseSources[0].ShareAllowed || m.caseSources[0].OriginalText != "askq:0:1" {
		t.Fatal(m)
	}
	if !m.ControlledFileWork || !m.FileWorkPrivate || m.ChannelID != q.Principal.ChatID {
		t.Fatal("callback changed original delivery scope")
	}
	if s.caseQuestion().Status != "done" || s.CaseSelection.Name != "林陈婚宴" {
		t.Fatal(s.caseQuestion(), s.CaseSelection)
	}
	if !e.handleCaseAnswer(p, answerFixture(q, "askq:0:1", true), "askq:0:1") || h.toolCalls != 2 {
		t.Fatal("duplicate executed")
	}
	if w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"case-result","input":{}}`); w.Code != 400 {
		t.Fatal("reconciliation exposed to model")
	}
}

func TestCaseQuestionLateAnswerAndAnotherQuestionCannotGrant(t *testing.T) {
	for _, reason := range []string{"late", "missing-time", "other-question"} {
		t.Run(reason, func(t *testing.T) {
			e, h, p, state, s := caseQuestionFixture(t)
			q := askFixtureCaseQuestion(t, e, state, s)
			h.toolResult = map[string]any{"status": "recorded"}
			m := answerFixture(q, "嗯", false)
			switch reason {
			case "late":
				m.UserMessageTimeMs = time.Now().Add(-time.Minute).UnixMilli()
			case "missing-time":
				m.UserMessageTimeMs = 0
			case "other-question":
				state.pending = &pendingPermission{RequestID: "other", Questions: []UserQuestion{{Question: "用暖色系吗？"}}}
			}
			e.handleCaseAnswer(p, m, m.Content)
			if h.toolCalls != 1 || s.caseQuestion().Status == "done" {
				t.Fatal("ambiguous/old answer granted operation", h.toolCalls, s.caseQuestion())
			}
			if reason == "other-question" && s.caseQuestion().Status != "cancelled" {
				t.Fatal("old question survived a competing question")
			}
		})
	}
}

func TestCaseQuestionRecoveredCancellationNeverRepublishes(t *testing.T) {
	for _, status := range []string{"awaiting", "executing"} {
		t.Run(status, func(t *testing.T) {
			e, h, p, state, s := caseQuestionFixture(t)
			q := *askFixtureCaseQuestion(t, e, state, s)
			q.Status = status
			if err := e.sessions.saveCaseQuestion(s, &q); err != nil {
				t.Fatal(err)
			}
			e.sessions = NewSessionManager(e.sessions.storePath)
			s = e.sessions.FindByID(s.ID)
			h.toolResult = map[string]any{"status": "not_executed", "current_revision": 1}
			if !e.handleCaseAnswer(p, answerFixture(&q, "取消", false), "取消") || s.caseQuestion().Status != "cancelled" || len(p.placeholders) != 1 {
				t.Fatal("cancelled question republished", s.caseQuestion())
			}
			if status == "executing" && h.toolCommand != "case-result" {
				t.Fatal("unknown result was not checked")
			}
		})
	}
}

func TestCaseQuestionRejectsOtherWorkActorQuotesAndStoppedAnswers(t *testing.T) {
	for _, reason := range []string{"actor", "other-work", "quote", "new-topic", "stop", "old-button"} {
		t.Run(reason, func(t *testing.T) {
			e, h, p, state, s := caseQuestionFixture(t)
			q := askFixtureCaseQuestion(t, e, state, s)
			m := answerFixture(q, "嗯", false)
			switch reason {
			case "actor":
				m.UserID = "someone-else"
			case "other-work":
				e.sessions.NewSession(m.SessionKey, "another work")
			case "quote":
				m.ParentMessageID = "foreign-work-artifact"
			case "new-topic":
				m.Content = "先帮我写一个私人备注"
			case "stop":
				if err := e.stopFileTurns(m.SessionKey); err != nil {
					t.Fatal(err)
				}
			case "old-button":
				m.InteractionRequestID = "case:stale:0"
				m.Content = "askq:0:1"
			}
			e.handleCaseAnswer(p, m, m.Content)
			if h.toolCalls != 1 {
				t.Fatal("unexpected execution", reason)
			}
			if reason == "new-topic" || reason == "quote" || reason == "stop" {
				e.handleCaseAnswer(p, answerFixture(q, "askq:0:1", true), "askq:0:1")
				if h.toolCalls != 1 {
					t.Fatal("cancelled question executed")
				}
			}
		})
	}
}

func TestCaseQuestionSaveFailureAndDefaultOffCannotPublishOrExecute(t *testing.T) {
	e, h, p, state, s := caseQuestionFixture(t)
	e.SetCaseConfirmationsEnabled(false)
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, questionToolRequest)
	if w.Code == 200 || s.caseQuestion() != nil || len(p.placeholders) != 0 {
		t.Fatal("default off created a question")
	}
	e.SetCaseConfirmationsEnabled(true)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = filepath.Join(parent, "sessions.json")
	w = actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, questionToolRequest)
	if w.Code == 200 || s.caseQuestion() != nil || len(p.placeholders) != 0 || h.toolCalls != 2 {
		t.Fatal("unsaved question acknowledged")
	}
}

func TestCaseQuestionRestartChecksResultAndRequiresFreshConfirmation(t *testing.T) {
	e, h, p, state, s := caseQuestionFixture(t)
	old := askFixtureCaseQuestion(t, e, state, s)
	e.sessions = NewSessionManager(e.sessions.storePath)
	s = e.sessions.FindByID(s.ID)
	h.toolResult = map[string]any{"status": "not_executed", "current_revision": 1}
	if !e.handleCaseAnswer(p, answerFixture(old, "嗯", false), "嗯") || h.toolCommand != "case-result" {
		t.Fatal("restart replayed operation")
	}
	fresh := s.caseQuestion()
	if fresh.ID == old.ID || fresh.Status != "awaiting" {
		t.Fatal(fresh)
	}
	if !e.handleCaseAnswer(p, answerFixture(old, "askq:0:1", true), "askq:0:1") || h.toolCalls != 2 {
		t.Fatal("old button survived restart")
	}
	h.toolResult = map[string]any{"status": "conflict", "current_revision": 2}
	e.handleCaseAnswer(p, answerFixture(fresh, "确认", false), "确认")
	if s.caseQuestion().Status != "needs_review" || h.toolCalls != 3 {
		t.Fatal("conflict replayed")
	}
}

func TestCaseQuestionContextUsesFrozenSourceNotModelConfirmation(t *testing.T) {
	e, _, _, state, s := caseQuestionFixture(t)
	q := askFixtureCaseQuestion(t, e, state, s)
	if q.Context.Confirmation != nil || q.Context.ShareAllowed {
		t.Fatal("original grant altered")
	}
	ctx, _, err := e.caseToolContext(context.Background(), state.actionToken, state.currentPrincipal, json.RawMessage(`{"confirmation":true}`))
	binding, _ := TrustedCaseContext(ctx)
	if err != nil || binding.Confirmation != nil {
		t.Fatal("model supplied approval")
	}
}

func TestCaseQuestionOldTextAnswerCannotConfirmNextQuestion(t *testing.T) {
	e, h, p, state, s := caseQuestionFixture(t)
	q := askFixtureCaseQuestion(t, e, state, s)
	h.toolResult = map[string]any{"status": "recorded", "case": map[string]any{"name": "林陈婚宴", "revision": 2}}
	m := answerFixture(q, "确认", false)
	e.handleCaseAnswer(p, m, "确认")
	h.toolResult = map[string]any{"status": "needs_confirmation", "question": "这次改成24桌吗？", "revision": 2}
	q = askFixtureCaseQuestion(t, e, state, s)
	before := h.toolCalls
	if !e.handleCaseAnswer(p, answerFixture(q, "确认", false), "确认") || h.toolCalls != before {
		t.Fatal("redelivered text answered another question")
	}
	if s.caseQuestion().Status != "awaiting" {
		t.Fatal("old answer consumed new question")
	}
	parent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	e.sessions.storePath = filepath.Join(parent, "sessions.json")
	m = answerFixture(q, "确认", false)
	m.MessageID = "new-answer"
	e.handleCaseAnswer(p, m, m.Content)
	if h.toolCalls != before || s.caseQuestion().Status != "awaiting" {
		t.Fatal("failed save executed operation")
	}
}
