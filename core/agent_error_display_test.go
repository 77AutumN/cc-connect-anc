package core

import (
	"errors"
	"testing"
	"time"
)

func TestUnsolicitedReader_BlockedOperationUsesLocalizedInterruption(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	sess := newControllableSession("blocked-operation")
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangChinese)
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	session := e.sessions.GetOrCreateActive("test:blocked:u1")
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.startUnsolicitedReader(state, session, e.sessions, "test:blocked:u1", "")
	state.mu.Lock()
	done := state.unsolicitedDone
	state.mu.Unlock()
	sess.events <- Event{Type: EventError, Error: errors.New("agent session could not complete the operation")}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("error reader did not stop")
	}
	if sent := p.getSent(); len(sent) != 1 || sent[0] != e.i18n.T(MsgResponseInterrupted) {
		t.Fatalf("expected localized outcome-unknown guidance, got %v", sent)
	}
}
