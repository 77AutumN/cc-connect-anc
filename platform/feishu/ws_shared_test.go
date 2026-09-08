package feishu

import (
	"context"
	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"testing"
)

func TestSharedWSGroup_RegisterAndAllPlatforms(t *testing.T) {
	// Clean up global state for test isolation.
	cleanup := func() {
		sharedWSMu.Lock()
		defer sharedWSMu.Unlock()
		for k := range sharedWSGroups {
			delete(sharedWSGroups, k)
		}
	}
	cleanup()
	defer cleanup()

	p1 := &Platform{appID: "cli_test", domain: "feishu.cn"}
	p2 := &Platform{appID: "cli_test", domain: "feishu.cn"}

	// Register first platform — should be primary.
	g1, isPrimary1 := registerSharedWS(p1)
	if !isPrimary1 {
		t.Fatal("first platform should be primary")
	}
	if len(g1.allPlatforms()) != 1 {
		t.Fatalf("expected 1 platform, got %d", len(g1.allPlatforms()))
	}

	// Register second platform — should be secondary, same group.
	g2, isPrimary2 := registerSharedWS(p2)
	if isPrimary2 {
		t.Fatal("second platform should not be primary")
	}
	if g1 != g2 {
		t.Fatal("both platforms should share the same group")
	}
	if len(g1.allPlatforms()) != 2 {
		t.Fatalf("expected 2 platforms, got %d", len(g1.allPlatforms()))
	}
}

func TestFixedGroupDropsMessagesUntilBotIsKnownAndMentioned(t *testing.T) {
	p := &Platform{platformName: "feishu", appID: "fixed-bot-identity", strictRoutes: true, allowFrom: "owner", allowChat: "group", dedup: &core.MessageDedup{}}
	if err := p.Prepare(func(core.Platform, *core.Message) { t.Error("unaddressed message reached core") }); err != nil {
		t.Fatal(err)
	}
	defer unregisterSharedWS(p)
	for _, bot := range []string{"", "verified-bot"} {
		p.botOpenID = bot
		msgType, chat, actor, chatType, content, id := "text", "group", "owner", "group", `{"text":"private discussion"}`, "message-"+bot
		err := p.onMessage(context.Background(), &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
			Sender:  &larkim.EventSender{SenderId: &larkim.UserId{OpenId: &actor}},
			Message: &larkim.EventMessage{MessageType: &msgType, ChatId: &chat, ChatType: &chatType, Content: &content, MessageId: &id},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedWSGroup_Unregister(t *testing.T) {
	cleanup := func() {
		sharedWSMu.Lock()
		defer sharedWSMu.Unlock()
		for k := range sharedWSGroups {
			delete(sharedWSGroups, k)
		}
	}
	cleanup()
	defer cleanup()

	p1 := &Platform{appID: "cli_test", domain: "feishu.cn"}
	p2 := &Platform{appID: "cli_test", domain: "feishu.cn"}

	g, _ := registerSharedWS(p1)
	registerSharedWS(p2)

	// Unregister first — one remains.
	remaining := unregisterSharedWS(p1)
	if remaining != 1 {
		t.Fatalf("expected 1 remaining, got %d", remaining)
	}
	platforms := g.allPlatforms()
	if len(platforms) != 1 || platforms[0] != p2 {
		t.Fatal("expected only p2 to remain")
	}

	// Unregister last — group deleted.
	remaining = unregisterSharedWS(p2)
	if remaining != 0 {
		t.Fatalf("expected 0 remaining, got %d", remaining)
	}
	sharedWSMu.Lock()
	_, exists := sharedWSGroups[sharedWSKey("cli_test", "feishu.cn")]
	sharedWSMu.Unlock()
	if exists {
		t.Fatal("group should be deleted when empty")
	}
}

func TestSharedWSGroup_DifferentAppIDs(t *testing.T) {
	cleanup := func() {
		sharedWSMu.Lock()
		defer sharedWSMu.Unlock()
		for k := range sharedWSGroups {
			delete(sharedWSGroups, k)
		}
	}
	cleanup()
	defer cleanup()

	p1 := &Platform{appID: "cli_aaa", domain: "feishu.cn"}
	p2 := &Platform{appID: "cli_bbb", domain: "feishu.cn"}

	g1, isPrimary1 := registerSharedWS(p1)
	g2, isPrimary2 := registerSharedWS(p2)

	if !isPrimary1 || !isPrimary2 {
		t.Fatal("different app_ids should each be primary")
	}
	if g1 == g2 {
		t.Fatal("different app_ids should have separate groups")
	}

	unregisterSharedWS(p1)
	unregisterSharedWS(p2)
}

func TestFixedRoutesRequireExactlyOneSenderAndChatBeforeReceiving(t *testing.T) {
	owner := &Platform{appID: "fixed-fixture", domain: "feishu.cn", strictRoutes: true, allowFrom: "owner", allowChat: "group"}
	partner := &Platform{appID: owner.appID, domain: owner.domain, strictRoutes: true, allowFrom: "partner", allowChat: "group"}
	private := &Platform{appID: owner.appID, domain: owner.domain, strictRoutes: true, allowFrom: "owner", allowChat: "private"}
	for _, p := range []*Platform{owner, partner, private} {
		if err := p.Prepare(func(core.Platform, *core.Message) {}); err != nil {
			t.Fatal(err)
		}
		defer unregisterSharedWS(p)
	}
	if !owner.acceptFixedRoute("owner", "group") || partner.acceptFixedRoute("owner", "group") || private.acceptFixedRoute("owner", "group") {
		t.Fatal("sender and actual chat must both match")
	}
	if owner.acceptFixedRoute("alt", "group") || private.acceptFixedRoute("owner", "other-private") {
		t.Fatal("unregistered route accepted")
	}
	if err := owner.Prepare(func(core.Platform, *core.Message) {}); err != nil {
		t.Fatal(err)
	}
	if len(owner.sharedGroup.allPlatforms()) != 3 {
		t.Fatal("prepare is not idempotent")
	}
	duplicate := &Platform{appID: owner.appID, domain: owner.domain, strictRoutes: true, allowFrom: "owner", allowChat: "group"}
	if err := duplicate.Prepare(func(core.Platform, *core.Message) {}); err == nil {
		t.Fatal("duplicate fixed route registered")
	}
}

func TestFixedRoutesThreePeopleSixEnvironments(t *testing.T) {
	var platforms []*Platform
	for _, actor := range []string{"owner", "partner", "test"} {
		for _, chat := range []string{"private-" + actor, "group"} {
			p := &Platform{appID: "three-people-fixture", domain: "feishu.cn", strictRoutes: true, allowFrom: actor, allowChat: chat}
			if err := p.Prepare(func(core.Platform, *core.Message) {}); err != nil {
				t.Fatal(err)
			}
			defer unregisterSharedWS(p)
			platforms = append(platforms, p)
		}
	}
	for _, actor := range []string{"owner", "partner", "test", "fourth-person"} {
		for _, chat := range []string{"private-owner", "private-partner", "private-test", "group", "other-group"} {
			matches := 0
			for _, p := range platforms {
				if p.acceptFixedRoute(actor, chat) {
					matches++
					if p.allowFrom != actor || p.allowChat != chat {
						t.Fatal("identity or actual chat changed at fixed route boundary")
					}
				}
			}
			want := 0
			if actor != "fourth-person" && (chat == "private-"+actor || chat == "group") {
				want = 1
			}
			if matches != want {
				t.Fatalf("route %s/%s matched %d environments, want %d", actor, chat, matches, want)
			}
		}
	}
	group := platforms[0].sharedGroup
	if len(group.allPlatforms()) != 6 {
		t.Fatal("six fixed routes must share one receiver group")
	}
	for _, p := range platforms {
		if p.sharedGroup != group {
			t.Fatal("additional receiver group created")
		}
	}
}
