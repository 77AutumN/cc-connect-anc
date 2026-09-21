package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fileHostStub struct {
	bindErr    error
	bindings   []FileWorkBinding
	toolWork   string
	toolErr    error
	toolResult map[string]any
}

func (h *fileHostStub) Bind(_ context.Context, b FileWorkBinding) (FileWorkContext, error) {
	h.bindings = append(h.bindings, b)
	return FileWorkContext{Enabled: true, WorkID: "fixture-work", WorkRoot: b.WorkRoot}, h.bindErr
}
func (h *fileHostStub) FindByMessage(context.Context, ActionPrincipal, string) (FileWorkRef, error) {
	return FileWorkRef{}, ErrFileWorkNotFound
}
func (h *fileHostStub) Tool(_ context.Context, _ string, _ json.RawMessage, _ ActionPrincipal, work string) (map[string]any, error) {
	h.toolWork = work
	if h.toolResult != nil {
		return h.toolResult, h.toolErr
	}
	return map[string]any{"status": "ok", "enabled": true, "work_id": work}, h.toolErr
}

type filePlatformStub struct{ stubPlatformEngine }

func (*filePlatformStub) SetFileWorkReplyObserver(func(Message, string) error) {}
func (*filePlatformStub) FileWorkReplyContext(raw json.RawMessage) (any, error) {
	return string(raw), nil
}
func (*fileHostStub) RecordReply(context.Context, ActionPrincipal, string) error { return nil }

func (p *filePlatformStub) SetFileWorkEnabled(bool, func(Message, string) bool) error { return nil }
func (p *filePlatformStub) FileReplyRoute(any) (json.RawMessage, error) {
	return json.RawMessage(`{"route":"fixture"}`), nil
}
func (p *filePlatformStub) SendFileWithReceipt(context.Context, json.RawMessage, FileAttachment, string) (string, error) {
	return "receipt", nil
}

type fileAgentStub struct {
	stubAgent
	attempts int
}

func (a *fileAgentStub) ForFileWork(string) (Agent, error) {
	a.attempts++
	return nil, errors.New("fixture runtime unavailable")
}

func TestFileWorkRejectsUnboundAndUnavailableRuntimeBeforeModel(t *testing.T) {
	p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
	a := &fileAgentStub{}
	e := NewEngine("fixture-project", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(e.cancel)
	h := &fileHostStub{}
	if err := e.SetFileWorkHost(h, func(id string) (string, int, error) { return filepath.Join(t.TempDir(), id), 0, nil }); err != nil {
		t.Fatal(err)
	}
	msg := Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", MessageID: "request", ControlledFileWork: true, ParentMessageID: "unknown", Content: "Revise the file"}
	// Unknown quotes arrive without downloading their files or text bodies.
	msg.Content = ""
	e.ReceiveMessage(p, &msg)
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "cannot be linked") {
		t.Fatalf("unknown work feedback: %v", got)
	}
	if len(h.bindings) != 0 || a.attempts != 0 {
		t.Fatal("unknown reply reached file or model runtime")
	}
	p.clearSent()
	msg.ParentMessageID = ""
	msg.Content = "Prepare the selected file"
	h.bindErr = errors.New("fixture disk failure")
	e.ReceiveMessage(p, &msg)
	if a.attempts != 0 || len(p.getSent()) != 1 {
		t.Fatal("failed original persistence reached model")
	}
	p.clearSent()
	h.bindErr = NewFileInputError(MsgFileInputFormatUnsupported)
	e.ReceiveMessage(p, &msg)
	if a.attempts != 0 || len(p.getSent()) != 1 || !strings.Contains(p.getSent()[0], "unsupported") {
		t.Fatal("unsupported format was reported as a storage failure")
	}
	p.clearSent()
	h.bindErr = nil
	msg.MessageID = "new-request"
	e.ReceiveMessage(p, &msg)
	if a.attempts != 1 || len(p.getSent()) != 1 || !strings.Contains(p.getSent()[0], "unavailable") {
		t.Fatal("unconfigured runtime did not fail closed")
	}
}

func TestFileWorkToolsUsePinnedWorkAndCurrentIdentity(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	t.Cleanup(e.cancel)
	h := &fileHostStub{}
	e.fileWorkHost = h
	state.fileWorkID = "host-work"
	body := `{"command":"work-context","input":{}}`
	call := func() int {
		return actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body).Code
	}
	if call() != 200 || h.toolWork != "host-work" {
		t.Fatal("tool lost host work binding")
	}
	state.currentPrincipal.UserID = "other"
	h.toolWork = ""
	if call() != 401 || h.toolWork != "" {
		t.Fatal("old token acquired another user's current identity")
	}
	state.currentPrincipal = state.actionPrincipal
	state.fileWorkID = ""
	if call() != 503 {
		t.Fatal("session without work acquired file capability")
	}
	state.fileWorkID = "host-work"
	h.toolErr = errors.New("private fixture path")
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "file_outcome_unconfirmed") || strings.Contains(w.Body.String(), "private") {
		t.Fatal("uncertain host failure exposed details or implied no send")
	}
	h.toolResult = map[string]any{"status": "blocked", "code": "file_version_conflict"}
	w = actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "file_version_conflict") || strings.Contains(w.Body.String(), "private") {
		t.Fatal("known pre-send refusal lost its safe category")
	}
	if w := actionToolRequest(e.ActionToolHandler(), "POST", "/send", state.actionToken, body); w.Code != 404 {
		t.Fatal("file transport exposed management route")
	}
}

func TestFileWorkDisabledAndLostSessionCounter(t *testing.T) {
	e := NewEngine("fixture", &stubAgent{}, nil, "", LangEnglish)
	t.Cleanup(e.cancel)
	if e.handleFileWorkMessage(nil, &Message{}) {
		t.Fatal("default behavior changed")
	}
	if err := e.SetFileWorkHost(&fileHostStub{}, func(string) (string, int, error) { return "", 0, nil }); err == nil {
		t.Fatal("unsupported runtime enabled")
	}
	one := &Session{ID: "s1", CreatedAt: time.Unix(1, 0)}
	two := &Session{ID: "s1", CreatedAt: time.Unix(2, 0)}
	if fileNativeSessionID(one) == fileNativeSessionID(two) {
		t.Fatal("lost session counter reused old work")
	}
}

func TestFilePrivateConversationContinuesUntilExplicitNewWork(t *testing.T) {
	p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
	a := &fileAgentStub{}
	e := NewEngine("fixture-project", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(e.cancel)
	h := &fileHostStub{}
	root := t.TempDir()
	if err := e.SetFileWorkHost(h, func(id string) (string, int, error) { return filepath.Join(root, id), 0, nil }); err != nil {
		t.Fatal(err)
	}
	msg := Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", ControlledFileWork: true, FileWorkPrivate: true}
	for i, content := range []string{"Prepare a banquet proposal", "Reduce the budget", "换个事，准备下周获客安排"} {
		msg.MessageID, msg.Content = fmt.Sprintf("request-%d", i), content
		e.ReceiveMessage(p, &msg)
	}
	if len(h.bindings) != 3 || h.bindings[0].SessionID != h.bindings[1].SessionID || h.bindings[1].SessionID == h.bindings[2].SessionID {
		t.Fatalf("private work continuation/new-work selection: %+v", h.bindings)
	}
	if len(e.sessions.ListSessions(msg.SessionKey)) != 2 {
		t.Fatal("new work discarded old session")
	}
	msg.FileWorkPrivate = false
	msg.Content = "Revise this"
	for _, id := range []string{"group-one", "group-two"} {
		msg.MessageID = id
		e.ReceiveMessage(p, &msg)
	}
	if h.bindings[3].SessionID == h.bindings[4].SessionID {
		t.Fatal("unlinked group messages inferred a recent work")
	}
}

func TestFileConversationIntentUsesOnlyExplicitUserClause(t *testing.T) {
	for text, want := range map[string]string{
		"换个事，做执行清单": "new", "另开一件事：做分析": "new", "新任务": "new",
		"先停下。": "stop", "停止当前工作": "stop", "先暂停": "stop",
		"他说“换个事”": "", "请把标题改为新任务": "", "先停下这段文字应该如何表达": "",
		"预算再降一点": "", "客户回复：\n换个事": "",
	} {
		if got := fileConversationIntent(text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}
