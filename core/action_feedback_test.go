package core

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestHostedReceiptCannotBeOverwrittenByFailedReplay(t *testing.T) {
	for _, code := range []string{"transport", "ledger_unavailable", "replayed", "known-failure", "binding-blocked"} {
		t.Run(code, func(t *testing.T) {
			host := &actionHostStub{}
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			e.SetActionHost(host)
			action := TrustedCardAction{Kind: host.Kind(), ApprovalID: "approval-fixture", Decision: ActionApprove}
			first := e.handleTrustedCardAction(p, action)
			final := first.Complete()
			if final.Card == nil {
				t.Fatal("missing verified receipt")
			}
			switch code {
			case "transport":
				host.claimErr = errors.New("helper stdout lost")
			case "ledger_unavailable":
				host.claimResult = &ActionHostResult{Status: "failed", Code: code, Card: NewCard().Title("stale failure", "red").Build()}
			case "replayed":
				host.claimResult = &ActionHostResult{Status: "verified", Replayed: true, Card: final.Card}
			case "known-failure":
				host.claimResult = &ActionHostResult{Status: "failed", Replayed: true, Card: NewCard().Title("Recorded prewrite failure", "red").Build()}
			case "binding-blocked":
				host.claimResult = &ActionHostResult{Status: "blocked", Code: "binding_changed", Card: NewCard().Title("stale blocked", "red").Build()}
			}
			late := e.handleTrustedCardAction(p, action)
			if late.Card != nil || late.Complete != nil || late.Toast == "" || len(host.executions) != 1 {
				t.Fatal("late duplicate can overwrite or repeat the completed operation")
			}
			if code == "known-failure" && late.Toast != "Recorded prewrite failure" {
				t.Fatal("known ledger failure mislabeled unknown")
			}
		})
	}
}

func TestHostedExecutionReceiptLostIsUnknownAndNeverRetried(t *testing.T) {
	host := &actionHostStub{executeErr: errors.New("committed then stdout lost")}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetActionHost(host)
	response := e.handleTrustedCardAction(p, TrustedCardAction{Kind: host.Kind(), ApprovalID: "approval-fixture", Decision: ActionApprove})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			final := response.Complete()
			if final.Card == nil || final.Card.HasButtons() || !final.Card.SharedUpdate || !strings.Contains(final.Card.RenderText(), "may already have completed") || !strings.Contains(final.Card.RenderText(), "approval-fixture") {
				t.Error("unknown result mislabeled or identity lost")
			}
		}()
	}
	wg.Wait()
	if len(host.executions) != 1 {
		t.Fatal("business operation was retried")
	}
}
