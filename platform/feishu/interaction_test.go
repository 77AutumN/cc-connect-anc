package feishu

import (
	"github.com/chenhg5/cc-connect/core"
	"testing"
)

func TestInteractionBindingRejectsOtherActorOldCardForgedOptionAndReplay(t *testing.T) {
	p := &Platform{}
	rc := replyContext{chatID: "chat", sessionKey: "feishu:chat:owner"}
	card := core.NewCard().Buttons(core.DefaultBtn("允许", "perm:allow")).Build()
	card.Interaction = &core.CardInteraction{RequestID: "current-question", Principal: core.ActionPrincipal{
		UserID: "owner", ChatID: rc.chatID, SessionKey: rc.sessionKey, Project: "owner-group",
	}}
	if err := p.bindInteraction("current-card", rc, card); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ user, chat, message, session, action string }{
		{"partner", "chat", "current-card", rc.sessionKey, "perm:allow"},
		{"owner", "other-chat", "current-card", rc.sessionKey, "perm:allow"},
		{"owner", "chat", "old-card", rc.sessionKey, "perm:allow"},
		{"owner", "chat", "current-card", "forged", "perm:allow"},
		{"owner", "chat", "current-card", rc.sessionKey, "perm:allow_all"},
	} {
		if _, _, ok := p.consumeInteraction(c.user, c.chat, c.message, c.session, c.action); ok {
			t.Fatal("unbound callback consumed question")
		}
	}
	id, _, ok := p.consumeInteraction("owner", "chat", "current-card", rc.sessionKey, "perm:allow")
	if !ok || id != "current-question" {
		t.Fatal("owner could not answer current question")
	}
	if _, _, ok := p.consumeInteraction("owner", "chat", "current-card", rc.sessionKey, "perm:allow"); ok {
		t.Fatal("replayed question consumed twice")
	}
}
