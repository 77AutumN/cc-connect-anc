package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileWorkRejectedIntakeHasCorrelatedTerminalBeforeModel(t *testing.T) {
	for _, tc := range []struct {
		name, parent, helper string
		bindErr              error
		reason               MsgKey
	}{
		{"unknown reply", "unknown", "", nil, MsgFileWorkAssociationRequired},
		{"save failure", "", "", errors.New("fixture disk error"), MsgFileInputSaveFailed},
		{"unsupported input", "", "", NewFileInputError(MsgFileInputFormatUnsupported), MsgFileInputSaveFailed},
		{"queue full", "", "queue-full", nil, MsgQueueFull},
		{"queue save failure", "", "queue-save", nil, MsgFileSupplementSaveFailed},
		{"recovery supplement save failure", "", "recover-save", nil, MsgFileSupplementSaveFailed},
		{"recovery continue save failure", "", "recover-continue", nil, MsgFileSupplementSaveFailed},
		{"ordinary queue full", "", "legacy-queue-full", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, err := os.CreateTemp(t.TempDir(), "journal-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := logs.Close(); err != nil {
					t.Error(err)
				}
			})
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
			defer slog.SetDefault(previous)
			p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
			a := &fileAgentStub{}
			e := NewEngine("fixture-project", a, []Platform{p}, "", LangEnglish)
			defer e.cancel()
			h := &fileHostStub{bindErr: tc.bindErr}
			if err := e.SetFileWorkHost(h, func(id string) (string, int, error) { return filepath.Join(t.TempDir(), id), 0, nil }); err != nil {
				t.Fatal(err)
			}
			msg := Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", MessageID: "request", ControlledFileWork: true, ParentMessageID: tc.parent, Content: "Prepare files"}
			if tc.helper == "" {
				e.ReceiveMessage(p, &msg)
			} else {
				turn := fixtureFileTurn(msg.MessageID)
				msg.fileTurn, msg.fileSession = &turn, e.sessions.GetOrCreateActive(msg.SessionKey)
				state := &interactiveState{fileWorkID: turn.WorkID}
				e.interactiveStates[msg.SessionKey] = state
				switch tc.helper {
				case "queue-full", "legacy-queue-full", "queue-save":
					if tc.helper != "queue-save" {
						e.maxQueuedMessages = 0
					}
					if tc.helper == "legacy-queue-full" {
						msg.ControlledFileWork = false
					}
					if !e.queueMessageForBusySession(p, &msg, msg.SessionKey) {
						t.Fatal("queue refusal not handled")
					}
				default:
					if tc.helper == "recover-continue" {
						msg.Content = "continue"
					}
					if !e.prepareFileRecovery(p, &msg, msg.fileSession, FileWorkContext{}) {
						t.Fatal("recovery save refusal not handled")
					}
				}
				if len(state.pendingMessages) != 0 || msg.fileSession.hasUnfinishedFileTurns() {
					t.Fatal("rejected intake was saved or queued")
				}
			}
			body, err := os.ReadFile(logs.Name())
			if err != nil {
				t.Fatal(err)
			}
			terminals := 0
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				if line == "" {
					continue
				}
				t.Log(line) // Synthetic journal rows also exercise the paired maintenance reader.
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event["msg"] == "turn complete" {
					t.Fatal("rejection reported as a completed model turn")
				}
				if event["msg"] == "file work intake rejected" {
					terminals++
					if event["msg_id"] != msg.MessageID || event["session"] != msg.SessionKey || event["platform"] != msg.Platform || event["reason"] != string(tc.reason) {
						t.Fatalf("uncorrelated rejection: %v", event)
					}
				}
			}
			wanted := 1
			if tc.reason == "" {
				wanted = 0
			}
			if terminals != wanted || a.attempts != 0 || len(p.getSent()) != 1 {
				t.Fatalf("rejection terminals=%d, model attempts=%d, replies=%d", terminals, a.attempts, len(p.getSent()))
			}
		})
	}
}

type fileHostStub struct {
	bindErr    error
	bindings   []FileWorkBinding
	toolWork   string
	toolErr    error
	toolResult map[string]any
}

type selectedInputHost struct {
	fileHostStub
	find func(ActionPrincipal, string) (FileWorkRef, error)
}

func (h *selectedInputHost) FindByMessage(ctx context.Context, principal ActionPrincipal, id string) (FileWorkRef, error) {
	if h.find != nil {
		return h.find(principal, id)
	}
	return h.fileHostStub.FindByMessage(ctx, principal, id)
}

func (h *selectedInputHost) Bind(ctx context.Context, b FileWorkBinding) (FileWorkContext, error) {
	work, err := h.fileHostStub.Bind(ctx, b)
	for _, input := range b.Inputs {
		work.IncomingInputs = append(work.IncomingInputs, FileWorkInput{ID: "selected", Name: input.FileName, Path: filepath.Join(b.WorkRoot, "inputs", input.FileName)})
	}
	return work, err
}

func TestFileWorkSelectedUploadDuplicateAfterRestartDoesNotRunAgain(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
			a := &fileAgentStub{}
			e := NewEngine("fixture-project", a, []Platform{p}, "", LangEnglish)
			t.Cleanup(e.cancel)
			path := filepath.Join(t.TempDir(), "sessions.json")
			e.sessions = NewSessionManager(path)
			turn := fixtureFileTurn("request")
			turn.Status = "completed"
			session := e.sessions.GetOrCreateActive(turn.Principal.SessionKey)
			if err := e.sessions.addFileTurn(session, turn, 2); err != nil {
				t.Fatal(err)
			}
			e.sessions = NewSessionManager(path)
			h := &selectedInputHost{find: func(principal ActionPrincipal, id string) (FileWorkRef, error) {
				if principal.UserID != "user" || principal.ChatID != "chat" || id != "request" {
					return FileWorkRef{}, ErrFileWorkNotFound
				}
				if uncertain {
					return FileWorkRef{}, ErrFileWorkUnconfirmed
				}
				return FileWorkRef{SessionID: fileNativeSessionID(session)}, nil
			}}
			if err := e.SetFileWorkHost(h, func(string) (string, int, error) { return t.TempDir(), 0, nil }); err != nil {
				t.Fatal(err)
			}
			msg := Message{Platform: "fixture", SessionKey: turn.Principal.SessionKey, UserID: "user", ChannelID: "chat", MessageID: "request", ParentMessageID: "standalone-upload", ControlledFileWork: true, FileWorkNewInput: true, Files: []FileAttachment{{FileName: "fictional.pdf", RequireSave: true}}}
			if !e.handleFileWorkMessage(p, &msg) || len(h.bindings) != 0 || a.attempts != 0 || len(e.sessions.ListSessions(msg.SessionKey)) != 1 {
				t.Fatal("replayed material selection started another work")
			}
			if uncertain && len(p.getSent()) != 1 {
				t.Fatal("unknown delivery lost its recovery hint")
			}
		})
	}
}

func TestFileWorkExplicitlySelectedUploadStartsOwnWork(t *testing.T) {
	for _, mode := range []string{"selected", "unverified", "private", "no-file", "save-failure", "busy"} {
		t.Run(mode, func(t *testing.T) {
			p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
			a := &fileAgentStub{}
			e := NewEngine("fixture-project", a, []Platform{p}, "", LangEnglish)
			t.Cleanup(e.cancel)
			h := &selectedInputHost{}
			if mode == "save-failure" {
				h.bindErr = NewFileInputError(MsgFileInputSaveFailed)
			}
			root := t.TempDir()
			if err := e.SetFileWorkHost(h, func(id string) (string, int, error) { return filepath.Join(root, id), 0, nil }); err != nil {
				t.Fatal(err)
			}
			msg := Message{Platform: "fixture", SessionKey: "fixture:chat:user", UserID: "user", ChannelID: "chat", MessageID: "request", ParentMessageID: "standalone-upload", ControlledFileWork: true, FileWorkNewInput: mode != "unverified", FileWorkPrivate: mode == "private", Content: "Use this file", Files: []FileAttachment{{FileName: "fictional.pdf", Data: []byte("fixture"), RequireSave: true}}}
			if mode == "no-file" {
				msg.Files = nil
			}
			if mode == "busy" {
				active := e.sessions.GetOrCreateActive(msg.SessionKey)
				if !active.TryLock() {
					t.Fatal("fixture session is already busy")
				}
				defer active.Unlock()
			}
			if !e.handleFileWorkMessage(p, &msg) {
				t.Fatal("selected upload escaped controlled intake")
			}
			if mode == "selected" || mode == "save-failure" {
				if len(h.bindings) != 1 || h.bindings[0].Principal.MessageID != "request" || h.bindings[0].SourceReceipt != "" || len(h.bindings[0].Inputs) != 1 || !h.bindings[0].Group {
					t.Fatal("material selection changed principal or imported another work")
				}
				if msg.ParentMessageID != "standalone-upload" {
					t.Fatal("original material reference was discarded")
				}
			} else if len(h.bindings) != 0 {
				t.Fatal("unverified selection created work")
			}
			if mode == "busy" && len(e.sessions.ListSessions(msg.SessionKey)) != 1 {
				t.Fatal("busy rejection changed the active work")
			}
			wantRuntime := 0
			if mode == "selected" {
				wantRuntime = 1
			}
			if a.attempts != wantRuntime {
				t.Fatal("failed material reached runtime", a.attempts)
			}
		})
	}
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
func (*fileHostStub) ActivateInputs(context.Context, ActionPrincipal, string) (FileWorkContext, error) {
	return FileWorkContext{}, nil
}

func (p *filePlatformStub) SetFileWorkEnabled(bool, func(Message, string) error) error { return nil }
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
	for i, content := range []string{"Prepare a banquet proposal", "Reduce the budget", "先换个事，准备下周获客安排"} {
		msg.MessageID, msg.Content = fmt.Sprintf("request-%d", i), content
		e.ReceiveMessage(p, &msg)
	}
	if len(h.bindings) != 3 || h.bindings[0].SessionID != h.bindings[1].SessionID || h.bindings[1].SessionID == h.bindings[2].SessionID {
		t.Fatalf("private work continuation/new-work selection: %+v", h.bindings)
	}
	if len(e.sessions.ListSessions(msg.SessionKey)) != 2 {
		t.Fatal("new work discarded old session")
	}
	current := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !current.TryLock() {
		t.Fatal("fixture is busy")
	}
	msg.MessageID, msg.Content = "busy-topic-change", "先换个事，查另一场婚宴"
	e.ReceiveMessage(p, &msg)
	current.UnlockWithoutUpdate()
	if len(h.bindings) != 3 || len(e.sessions.ListSessions(msg.SessionKey)) != 2 {
		t.Fatal("busy topic change was bound or queued as a supplement")
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
		"先换个事，林陈婚宴目前多少桌？": "new", "先换一件事：只查进度": "new",
		"先别换个事": "", "不要先换个事": "", "他说：先换个事": "",
		"先换个事可以吗？": "", "请把标题改成先换个事": "",
		"先停下。": "stop", "停止当前工作": "stop", "先暂停": "stop",
		"他说“换个事”": "", "请把标题改为新任务": "", "先停下这段文字应该如何表达": "",
		"预算再降一点": "", "客户回复：\n换个事": "",
	} {
		if got := fileConversationIntent(text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
}
