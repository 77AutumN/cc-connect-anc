package files

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestTextReplyAssociationPreservesScopeAndOriginalRoute(t *testing.T) {
	f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) { return "", nil })
	p := f.binding.Principal
	if err := f.host.RecordReply(context.Background(), p, "bot-answer"); err != nil {
		t.Fatal(err)
	}
	ref, err := f.host.FindByMessage(context.Background(), p, "bot-answer")
	if err != nil || ref.WorkID != f.context.WorkID || ref.SessionID != f.binding.SessionID {
		t.Fatal("reply did not resolve to original work", err)
	}
	for _, mutate := range []func(*core.ActionPrincipal){
		func(p *core.ActionPrincipal) { p.UserID = "other-staff" },
		func(p *core.ActionPrincipal) { p.ChatID = "other-chat" },
		func(p *core.ActionPrincipal) { p.MessageID = "unbound-message" },
	} {
		other := p
		mutate(&other)
		if f.host.RecordReply(context.Background(), other, "foreign-answer") == nil {
			t.Fatal("unbound principal recorded a reply")
		}
	}
	if _, err := f.host.FindByMessage(context.Background(), p, "foreign-answer"); err == nil {
		t.Fatal("failed association was persisted")
	}
}

func TestPendingAttachmentIsHostOnlyUntilItsTurnAndActivationIsIdempotent(t *testing.T) {
	f := newFixture(t, nil)
	b := f.binding
	b.Principal.MessageID = "supplement"
	b.DeferInputs = true
	data := packageBytes(t, documentParts("xlsx"))
	b.Inputs = []core.FileAttachment{{FileName: "fictional.xlsx", Data: data}}
	pending, err := f.host.Bind(context.Background(), b)
	if err != nil || len(pending.Inputs) != 0 || len(pending.IncomingInputs) != 1 {
		t.Fatal("pending inputs exposed", err)
	}
	input := pending.IncomingInputs[0]
	if _, err := os.Stat(input.Path); !os.IsNotExist(err) {
		t.Fatal("pending file exposed in model directory")
	}
	foreign := b.Principal
	foreign.UserID = "another-employee"
	if _, err := f.host.ActivateInputs(context.Background(), foreign, pending.WorkID); err == nil {
		t.Fatal("foreign employee activated inputs")
	}
	// Simulate a crash after the file copy and before committing the host index.
	if err := os.WriteFile(input.Path, data, 0440); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := f.host.ActivateInputs(context.Background(), b.Principal, pending.WorkID)
		if err != nil || len(got.Inputs) != 1 || got.Inputs[0] != input {
			t.Fatal("activation duplicated or lost preserved input", err)
		}
	}
}
