package core

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUnsolicitedReader_EmptySessionMetadataDoesNotStartTurn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		events      []Event
		want        string
		interrupted bool
	}{
		{name: "metadata_then_close"},
		{
			name:   "metadata_then_result",
			events: []Event{{Type: EventResult, Content: "Completed draft", Done: true}},
			want:   "Completed draft",
		},
		{
			name:        "real_text_then_close",
			events:      []Event{{Type: EventText, Content: "Preparing draft"}},
			interrupted: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			agentSession := newControllableSession("metadata-session")
			e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
			e.SetDisplayConfig(DisplayCfg{FinalResponseOnly: true})
			t.Cleanup(func() { _ = e.Stop() })
			key := "test:metadata:user"
			session := e.sessions.GetOrCreateActive(key)
			state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}

			// Adapters use an empty text event to announce a session ID. It
			// carries no business output and may arrive between user turns.
			agentSession.events <- Event{Type: EventText, SessionID: "metadata-session"}
			for _, event := range tc.events {
				agentSession.events <- event
			}
			close(agentSession.events)
			e.startUnsolicitedReader(state, session, e.sessions, key, "")
			t.Cleanup(func() { e.stopUnsolicitedReader(state) })
			state.mu.Lock()
			done := state.unsolicitedDone
			state.mu.Unlock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("unsolicited reader did not finish")
			}

			want := tc.want
			if tc.interrupted {
				want = e.i18n.T(MsgResponseInterrupted)
			}
			sent, history := p.getSent(), session.GetHistory(0)
			if want == "" {
				if len(sent) != 0 || len(history) != 0 {
					t.Fatalf("metadata created a false turn: sent=%q history=%v", sent, history)
				}
			} else if len(sent) != 1 || sent[0] != want || len(history) != 1 || history[0].Content != want {
				t.Fatalf("unexpected business output: sent=%q history=%v, want %q", sent, history, want)
			}
		})
	}
}
