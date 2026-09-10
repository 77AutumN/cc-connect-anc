package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestReminderSendPinsRouteUUIDAndReceipt(t *testing.T) {
	var bodies []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fixture-reminder-token"})
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages" || r.URL.Query().Get("receive_id_type") != "chat_id" {
			t.Errorf("unexpected destination API")
			w.WriteHeader(404)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "accepted-fixture"}})
	}))
	defer server.Close()
	p := &Platform{strictRoutes: true, allowChat: "private-fixture", client: lark.NewClient("reminder-test-app", "fixture-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	for i := 0; i < 2; i++ {
		id, err := p.SendReminder(context.Background(), "private-fixture", "fixed body", "fixed-uuid")
		if err != nil || id != "accepted-fixture" {
			t.Fatalf("send: %s %v", id, err)
		}
	}
	if len(bodies) != 2 || bodies[0]["uuid"] != "fixed-uuid" || bodies[0]["content"] != bodies[1]["content"] || bodies[0]["receive_id"] != "private-fixture" {
		t.Fatal("frozen payload changed")
	}
	if _, err := p.SendReminder(context.Background(), "other-chat", "fixed body", "fixed-uuid"); !errors.Is(err, core.ErrReminderPermission) || len(bodies) != 2 {
		t.Fatal("alternate destination accepted")
	}
}
