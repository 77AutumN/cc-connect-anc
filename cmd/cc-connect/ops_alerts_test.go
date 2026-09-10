package main

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/reminders"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type opsTestSender struct{}

func (opsTestSender) SendReminder(context.Context, string, string, string) (string, error) {
	return "fixture", nil
}

func TestOperationsRouteSurvivesUnavailableNonOwnerToolHost(t *testing.T) {
	cfg := &config.Config{}
	verified := map[string]bool{}
	availableTools := map[string]bool{}
	for _, user := range []string{"owner", "test", "member"} {
		for _, kind := range []string{"private", "group"} {
			name := user + "-" + kind
			chat := "group"
			if kind == "private" {
				chat = user + "-private-chat"
			}
			cfg.Projects = append(cfg.Projects, config.ProjectConfig{Name: name, Platforms: []config.PlatformConfig{{Type: "feishu", Options: map[string]any{"allow_chat": chat, "allow_from": user}}}})
			verified[name] = true
			if name != "test-group" {
				availableTools[name] = true
			}
		}
	}
	if _, err := reminderRoutes(cfg, availableTools); err == nil {
		t.Fatal("incomplete reminder tools accepted")
	}
	identityRoutes, err := reminderRoutes(cfg, verified)
	if err != nil {
		t.Fatal(err)
	}
	senders := map[string]reminders.Sender{"owner": authorizedReminderSender{engine: &core.Engine{}, user: "owner", sender: opsTestSender{}}}
	recipient := operationsRecipient("owner-private", identityRoutes, senders)()
	if recipient.Sender == nil || recipient.Chat != "owner-private-chat" {
		t.Fatal("source tool failure disabled verified Owner alert destination")
	}
}

func TestOperationsRecipientRequiresUniqueVerifiedPrivateProject(t *testing.T) {
	routes := []reminders.Route{{User: "a", Chat: "pa", Project: "owner-private", PrivateChat: "pa"}, {User: "a", Chat: "group", Project: "owner-group", PrivateChat: "pa"}}
	senders := map[string]reminders.Sender{"a": authorizedReminderSender{engine: &core.Engine{}, user: "a", sender: opsTestSender{}}}
	selected := operationsRecipient("owner-private", routes, senders)()
	if selected.Sender == nil || selected.Chat != "pa" || selected.Binding == "" {
		t.Fatal("verified owner not selected")
	}
	owner := senders["a"].(authorizedReminderSender)
	owner.engine.SetUserRoles(core.NewUserRoleManager())
	revoked := operationsRecipient("owner-private", routes, senders)()
	if revoked.Sender != nil || revoked.Binding != selected.Binding || revoked.Chat != selected.Chat {
		t.Fatal("revocation changed the frozen binding or retained sending")
	}
	owner.engine.SetUserRoles(nil)
	for _, project := range []string{"", "unknown", "owner-group"} {
		if got := operationsRecipient(project, routes, senders)(); got.Sender != nil || got.Binding != "" {
			t.Fatal("unverified destination accepted", project)
		}
	}
	duplicate := append(append([]reminders.Route{}, routes...), routes[0])
	if operationsRecipient("owner-private", duplicate, senders)().Sender != nil {
		t.Fatal("ambiguous recipient accepted")
	}
	senders["a"] = opsTestSender{}
	if operationsRecipient("owner-private", routes, senders)().Sender != nil {
		t.Fatal("unwrapped authorization bypass")
	}
}
