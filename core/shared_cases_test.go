package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaseSharingUsesOnlyExplicitOriginalParagraph(t *testing.T) {
	e, _, _, _ := actionToolFixture(t)
	t.Cleanup(e.cancel)
	msg := Message{ControlledFileWork: true, FileWorkPrivate: true, MessageID: "own", Content: "请同步给执行：林陈婚宴改为23桌\n这段私聊不共享", ExtraContent: "同步给执行：工资50000"}
	e.captureCaseSource(&msg)
	if len(msg.caseSources) != 0 {
		t.Fatal("default off captured a source")
	}
	e.SetSharedCasesEnabled(true)
	e.captureCaseSource(&msg)
	if got := msg.caseSources[0]; !got.ShareAllowed || got.Text != "林陈婚宴改为23桌" {
		t.Fatal(got)
	}
	for _, text := range []string{"不要同步给执行：23桌", "是否同步给执行：23桌？", "客户说‘同步给执行：23桌’", "如果同步给执行：23桌", "同步给执行了吗？", "讨论23桌", "继续", "同步给执行：林陈婚宴23桌\n不要同步，刚才说错了"} {
		msg.Content = text
		e.captureCaseSource(&msg)
		if got := msg.caseSources[0]; got.ShareAllowed || got.Text != text {
			t.Fatalf("private disclosure %q: %+v", text, got)
		}
	}
	msg.FileWorkPrivate = false
	msg.Content = "林陈婚宴22桌"
	e.captureCaseSource(&msg)
	if got := msg.caseSources[0]; !got.ShareAllowed || got.Text != msg.Content {
		t.Fatal(got)
	}
	msg.Content = "林陈婚宴23桌，先不要记入"
	e.captureCaseSource(&msg)
	if msg.caseSources[0].ShareAllowed {
		t.Fatal("group denial ignored")
	}
}

func TestCaseToolsFailClosedOnDisabledStaleOrForgedSource(t *testing.T) {
	e, h, _, state := actionToolFixture(t)
	t.Cleanup(e.cancel)
	state.fileWorkID = "fixture-work"
	state.caseSources = []CaseSource{{MessageID: "request", Text: "林陈婚宴22桌", ShareAllowed: true}}
	body := `{"command":"case-read","input":{"case_name":"林陈婚宴"}}`
	if w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body); w.Code != 403 || h.toolCalls != 0 {
		t.Fatal(w.Body.String())
	}
	e.SetSharedCasesEnabled(true)
	for _, command := range []string{"case-artifact", "host-case-import"} {
		body := `{"command":"` + command + `","input":{}}`
		if w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body); w.Code != 400 {
			t.Fatal("host-only command exposed", command)
		}
	}
	p := state.currentPrincipal
	ctx, got, err := e.caseToolContext(context.Background(), state.actionToken, p, json.RawMessage(`{}`))
	binding, ok := TrustedCaseContext(ctx)
	if err != nil || !ok || got != p || binding.WorkID != state.fileWorkID || !binding.ShareAllowed {
		t.Fatal(binding, err)
	}
	if _, _, err := e.caseToolContext(ctx, state.actionToken, p, json.RawMessage(`{"source_message_id":"foreign"}`)); err == nil {
		t.Fatal("foreign source granted")
	}
	state.currentPrincipal.MessageID = "next"
	if _, _, err := e.caseToolContext(ctx, state.actionToken, p, json.RawMessage(`{}`)); err == nil {
		t.Fatal("old request borrowed next turn")
	}
	state.currentPrincipal = p
	if w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, body); w.Code != 200 || h.toolCalls != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestCaseSelectionPersistsAndIsPinnedAtArrivalForOnlyThisEmployee(t *testing.T) {
	e, _, _, state := actionToolFixture(t)
	t.Cleanup(e.cancel)
	path := filepath.Join(t.TempDir(), "sessions.json")
	e.sessions = NewSessionManager(path)
	p := state.currentPrincipal
	e.sessions.GetOrCreateActive(p.SessionKey)
	e.rememberCase(p, map[string]any{"case": map[string]any{"name": "林陈婚宴"}})
	e.sessions = NewSessionManager(path)
	e.SetSharedCasesEnabled(true)
	msg := Message{ControlledFileWork: true, Platform: p.Platform, UserID: p.UserID, ChannelID: p.ChatID, SessionKey: p.SessionKey, MessageID: "supplement", Content: "桌数改为23桌"}
	e.captureCaseSource(&msg)
	if len(msg.caseSources) != 1 || msg.caseSources[0].SelectedCase != "林陈婚宴" {
		t.Fatal(msg.caseSources)
	}
	e.sessions.NewSession(p.SessionKey, "another work")
	e.rememberCase(p, map[string]any{"case": map[string]any{"name": "周许婚宴"}})
	if msg.caseSources[0].SelectedCase != "林陈婚宴" {
		t.Fatal("queued selection changed with active work")
	}
	msg.UserID = "other-employee"
	e.captureCaseSource(&msg)
	if msg.caseSources[0].SelectedCase != "" {
		t.Fatal("inherited another employee's association")
	}
}

func TestCaseSourcesPersistPerSupplementAndRecoveryKeepsOriginalGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
	e := NewEngine("project", &fileAgentStub{}, []Platform{p}, path, LangEnglish)
	t.Cleanup(e.cancel)
	e.SetSharedCasesEnabled(true)
	s := e.sessions.GetOrCreateActive("fixture:chat:user")
	for _, id := range []string{"shared", "private"} {
		turn := fixtureFileTurn(id)
		turn.CaseSources = []CaseSource{{MessageID: id, ShareAllowed: id == "shared"}}
		if id == "shared" {
			turn.CaseSources[0].Text = "林陈婚宴23桌"
		}
		if err := e.sessions.addFileTurn(s, turn, 3); err != nil {
			t.Fatal(err)
		}
	}
	e.sessions = NewSessionManager(path)
	s = e.sessions.FindByID(s.ID)
	turns := s.fileTurns()
	if len(turns) != 2 || !turns[0].CaseSources[0].ShareAllowed || turns[1].CaseSources[0].ShareAllowed {
		t.Fatal(turns)
	}
	turns[0].CaseSources[0].Text = "mutated"
	if s.fileTurns()[0].CaseSources[0].Text == "mutated" {
		t.Fatal("snapshot aliases persisted grants")
	}
	current := fixtureFileTurn("resume")
	msg := &Message{Content: "继续", MessageID: "resume", fileTurn: &current}
	if e.prepareFileRecovery(p, msg, s, FileWorkContext{WorkID: "work"}) {
		t.Fatal("recovery rejected")
	}
	if len(msg.caseSources) != 2 || !msg.caseSources[0].ShareAllowed || msg.caseSources[1].ShareAllowed || !strings.Contains(msg.Content, "source_message_id") {
		t.Fatal(msg.caseSources)
	}
}
