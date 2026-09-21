package files

import (
	"context"
	"encoding/json"
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
