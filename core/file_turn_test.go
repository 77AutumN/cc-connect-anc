package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixtureFileTurn(id string) FileTurn {
	return FileTurn{Principal: ActionPrincipal{MessageID: id, Platform: "fixture", UserID: "user", ChatID: "chat", SessionKey: "fixture:chat:user", Project: "project"},
		WorkID: "work", Content: "fictional supplement", Route: json.RawMessage(`{"fixture":true}`), Status: "queued"}
}

func TestFileTurnPersistsBeforeAcknowledgmentAndRetainsCrashStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sm := NewSessionManager(path)
	s := sm.GetOrCreateActive("fixture:chat:user")
	turn := fixtureFileTurn("supplement")
	if err := sm.addFileTurn(s, turn, 2); err != nil {
		t.Fatal(err)
	}
	reloaded := NewSessionManager(path).GetOrCreateActive(turn.Principal.SessionKey)
	if got := reloaded.fileTurns(); len(got) != 1 || got[0].Status != "queued" {
		t.Fatal("accepted supplement lost", got)
	}
	if err := sm.setFileTurnStatus(s, turn.Principal.MessageID, "started"); err != nil {
		t.Fatal(err)
	}
	reloaded = NewSessionManager(path).GetOrCreateActive(turn.Principal.SessionKey)
	if got := reloaded.fileTurns(); len(got) != 1 || got[0].Status != "started" {
		t.Fatal("in-flight supplement was deleted", got)
	}
	if err := sm.addFileTurn(s, turn, 2); err != nil {
		t.Fatal(err)
	}
	if len(s.fileTurns()) != 1 {
		t.Fatal("transport duplicate admitted twice")
	}
	if sm.DeleteByID(s.ID) || sm.PruneEmptySessions() != 0 {
		t.Fatal("unfinished supplement pruned")
	}
	if err := sm.setFileTurnStatus(s, turn.Principal.MessageID, "completed"); err != nil {
		t.Fatal(err)
	}
	if s.hasUnfinishedFileTurns() {
		t.Fatal("completed turn remained actionable")
	}
}

func TestFileTurnStorageFailureIsReportedAndCorruptionPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sm := NewSessionManager(path)
	s := sm.GetOrCreateActive("fixture:chat:user")
	// A directory at the destination produces a real atomic-replace failure on
	// both platforms, without relying on root-bypassable mode bits.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if sm.addFileTurn(s, fixtureFileTurn("failed"), 1) == nil || len(s.fileTurns()) != 0 {
		t.Fatal("failed write accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	broken := []byte(`{"sessions":`)
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	sm = NewSessionManager(path)
	s = sm.GetOrCreateActive("fixture:chat:user")
	if sm.addFileTurn(s, fixtureFileTurn("after-corruption"), 1) == nil {
		t.Fatal("corrupt store accepted supplement")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(broken) {
		t.Fatal("corrupt evidence replaced")
	}
}

func TestFileTurnConcurrentSavesCannotUndoAcceptedSupplement(t *testing.T) {
	sm := NewSessionManager(filepath.Join(t.TempDir(), "sessions.json"))
	s := sm.GetOrCreateActive("fixture:chat:user")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); sm.Save() }()
	}
	if err := sm.addFileTurn(s, fixtureFileTurn("accepted"), 1); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(NewSessionManager(sm.StorePath()).FindByID(s.ID).fileTurns()) != 1 {
		t.Fatal("older save overwrote supplement")
	}
	if sm.addFileTurn(s, fixtureFileTurn("overflow"), 1) == nil {
		t.Fatal("unbounded pending supplements")
	}
}

func TestFileStopPreservesOtherWorkSupplements(t *testing.T) {
	e := NewEngine("project", &fileAgentStub{}, nil, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	key := "fixture:chat:user"
	old := e.sessions.GetOrCreateActive(key)
	current := e.sessions.NewSession(key, "another work")
	for _, s := range []*Session{old, current} {
		if err := e.sessions.addFileTurn(s, fixtureFileTurn(s.ID), 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.stopFileTurns(key); err != nil {
		t.Fatal(err)
	}
	if !old.hasUnfinishedFileTurns() || current.hasUnfinishedFileTurns() {
		t.Fatal("stop crossed the current work boundary")
	}
}

func TestFileRecalledSupplementRemainsStoppedAfterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("project", &fileAgentStub{}, nil, path, LangEnglish)
	t.Cleanup(e.cancel)
	turn := fixtureFileTurn("recalled")
	key := turn.Principal.SessionKey
	s := e.sessions.GetOrCreateActive(key)
	if err := e.sessions.addFileTurn(s, turn, 2); err != nil {
		t.Fatal(err)
	}
	e.interactiveStates[key] = &interactiveState{fileWorkID: turn.WorkID, pendingMessages: []queuedMessage{{messageID: "recalled", fileWorkID: turn.WorkID}}}
	if _, ok := e.removeQueuedMessageByID("recalled"); !ok {
		t.Fatal("recall not applied")
	}
	if NewSessionManager(path).FindByID(s.ID).hasUnfinishedFileTurns() {
		t.Fatal("recalled supplement restored")
	}
}

func TestFileQueueAcknowledgesOnlyAfterDurableSave(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "disk failure"}[fail], func(t *testing.T) {
			p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
			path := filepath.Join(t.TempDir(), "sessions.json")
			e := NewEngine("project", &fileAgentStub{}, []Platform{p}, path, LangEnglish)
			t.Cleanup(e.cancel)
			turn := fixtureFileTurn("supplement")
			key := turn.Principal.SessionKey
			s := e.sessions.GetOrCreateActive(key)
			state := &interactiveState{fileWorkID: turn.WorkID}
			e.interactiveStates[key] = state
			if fail {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			accepted := false
			msg := &Message{SessionKey: key, MessageID: "supplement", Content: turn.Content, fileTurn: &turn, fileSession: s,
				OnAccepted: func() {
					accepted = true
					if got := NewSessionManager(path).FindByID(s.ID); got == nil || len(got.fileTurns()) != 1 {
						t.Error("acknowledged before durable receipt")
					}
				}}
			if !e.queueMessageForBusySession(p, msg, key) {
				t.Fatal("not handled")
			}
			if accepted == fail || (len(state.pendingMessages) == 0) != fail {
				t.Fatal("ack/queue does not match persistence")
			}
			feedback := strings.Join(p.getSent(), " ")
			if fail && !strings.Contains(feedback, e.i18n.T(MsgFileSupplementSaveFailed)) {
				t.Fatal("missing save failure feedback")
			}
			if !fail && !strings.Contains(feedback, e.i18n.T(MsgFileSupplementQueued)) {
				t.Fatal("missing next-version acknowledgment")
			}
		})
	}
}

func TestFileRecoveryRequiresExplicitContinueAndStopsOnUnknownDelivery(t *testing.T) {
	p := &filePlatformStub{stubPlatformEngine: stubPlatformEngine{n: "fixture"}}
	e := NewEngine("project", &fileAgentStub{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	s := e.sessions.GetOrCreateActive("fixture:chat:user")
	turn := fixtureFileTurn("before-restart")
	turn.Status = "started"
	if err := e.sessions.addFileTurn(s, turn, 3); err != nil {
		t.Fatal(err)
	}
	current := fixtureFileTurn("after-restart")
	msg := &Message{Content: "continue", MessageID: "after-restart", fileTurn: &current}
	work := FileWorkContext{WorkID: "work", Artifacts: []FileWorkArtifact{{Status: "unknown"}}}
	if !e.prepareFileRecovery(p, msg, s, work) || !strings.Contains(strings.Join(p.getSent(), " "), "unconfirmed") {
		t.Fatal("uncertain delivery replayed")
	}
	work.Artifacts = nil
	if e.prepareFileRecovery(p, msg, s, work) || !strings.Contains(msg.Content, "fictional supplement") || !strings.Contains(msg.Content, "reconcile") {
		t.Fatal("recovery omitted reconciliation")
	}
	if err := e.sessions.setFileTurnStatus(s, msg.MessageID, "completed"); err != nil {
		t.Fatal(err)
	}
	if s.hasUnfinishedFileTurns() {
		t.Fatal("reconciliation left the same requirement replayable")
	}
	if err := e.sessions.addFileTurn(s, fixtureFileTurn("to-stop"), 3); err != nil {
		t.Fatal(err)
	}
	if err := e.stopFileTurns("fixture:chat:user"); err != nil {
		t.Fatal(err)
	}
	if err := e.sessions.setFileTurnStatus(s, "to-stop", "completed"); err != nil {
		t.Fatal(err)
	}
	if got := s.fileTurns(); got[len(got)-1].Status != "stopped" {
		t.Fatal("late completion revived a stopped requirement")
	}
}
