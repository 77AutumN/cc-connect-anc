package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// Exercise the actual placeholder -> PATCH -> authenticated callback adapter;
// the engine's fake card sender cannot catch a missing Feishu interaction bind.
func TestRefreshedBusinessQuestionBindsExactCallback(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "shared"}[shared], func(t *testing.T) {
			failPatch := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/tenant_access_token/internal"):
					writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fictional"})
				case strings.HasSuffix(r.URL.Path, "/reply"):
					writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "question-card"}})
				case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/question-card"):
					code := 0
					if failPatch {
						code = 230001
					}
					writeJSON(t, w, map[string]any{"code": code, "msg": "synthetic patch result"})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			p := &interactivePlatform{Platform: &Platform{
				platformName: "feishu", domain: srv.URL, appID: t.Name(),
				strictRoutes: true, allowFrom: "owner", allowChat: "chat",
				client: lark.NewClient(t.Name(), "fictional", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client())),
			}}
			messages := make(chan *core.Message, 2)
			if err := p.Prepare(func(_ core.Platform, m *core.Message) { messages <- m }); err != nil {
				t.Fatal(err)
			}
			defer unregisterSharedWS(p.Platform)
			p.userNameCache.Store("owner", "Employee")
			p.chatNameCache.Store("chat", "Test chat")
			rc := replyContext{messageID: "original-request", chatID: "chat", sessionKey: "feishu:chat:owner"}
			id, err := p.ReplyHostedActionPlaceholder(context.Background(), rc, core.NewCard().PlainText("Preparing").Build())
			if err != nil {
				t.Fatal(err)
			}
			card := core.NewCard().Buttons(core.CardButton{Text: "Confirm", Value: "askq:0:1", Extra: map[string]string{"askq_label": "Confirm"}}).Build()
			card.SharedUpdate = shared
			card.Interaction = &core.CardInteraction{RequestID: "case:question:0", Principal: core.ActionPrincipal{UserID: "owner", ChatID: rc.chatID, SessionKey: rc.sessionKey}}
			if err := p.RefreshCardMessage(context.Background(), id, rc.sessionKey, card); err != nil {
				t.Fatal(err)
			}
			click := func(user, chat, receipt, action string) *callback.CardActionTriggerResponse {
				t.Helper()
				response, err := p.onCardAction(&callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
					Operator: &callback.Operator{OpenID: user}, Context: &callback.Context{OpenChatID: chat, OpenMessageID: receipt},
					Action: &callback.CallBackAction{Value: map[string]any{"action": action, "session_key": "forged", "askq_label": "forged"}},
				}})
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			for _, input := range [][4]string{
				{"other", "chat", id, "askq:0:1"}, {"owner", "other-chat", id, "askq:0:1"},
				{"owner", "chat", "old-card", "askq:0:1"}, {"owner", "chat", id, "askq:0:9"},
			} {
				if click(input[0], input[1], input[2], input[3]) != nil {
					t.Fatal("unbound answer accepted")
				}
			}
			if response := click("owner", "chat", id, "askq:0:1"); response == nil || response.Card == nil {
				t.Fatal("published business question button was silently dropped")
			}
			select {
			case msg := <-messages:
				if msg.InteractionRequestID != card.Interaction.RequestID || msg.SessionKey != rc.sessionKey || msg.Content != "askq:0:1" {
					t.Fatalf("callback lost trusted question: %+v", msg)
				}
			case <-time.After(time.Second):
				t.Fatal("confirmed answer did not reach engine")
			}
			if click("owner", "chat", id, "askq:0:1") != nil {
				t.Fatal("duplicate answer accepted")
			}
			failPatch = true
			card.Interaction.RequestID = "case:unpublished:0"
			if err := p.RefreshCardMessage(context.Background(), id, rc.sessionKey, card); err == nil {
				t.Fatal("failed PATCH reported success")
			}
			if click("owner", "chat", id, "askq:0:1") != nil {
				t.Fatal("failed publication installed a new answer binding")
			}
		})
	}
}
