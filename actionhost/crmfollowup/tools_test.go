package crmfollowup

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestToolsModeInitializesWithoutInboxAndDoesNotInterceptPermissions(t *testing.T) {
	workDir := t.TempDir()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{}
	if err := a.EnableTools(); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkDir(workDir, account.Username); err != nil {
		t.Fatalf("tool mode still requires the legacy inbox: %v", err)
	}
	env, err := a.SessionEnv("opaque-token")
	if err != nil || len(env) != 1 || env[0] != actionTokenEnv+"=opaque-token" {
		t.Fatalf("session environment = %v, %v", env, err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".crm-input")); !os.IsNotExist(err) {
		t.Fatalf("tool mode created an inbox: %v", err)
	}
	event := core.Event{Type: core.EventPermissionRequest, ToolName: "Bash", ToolInputRaw: map[string]any{"command": markerCommand + " request-approval"}}
	if _, matched := a.Match(event); matched {
		t.Fatal("tool mode intercepted the legacy permission marker")
	}
	if _, err := a.Begin(context.Background(), core.ActionRef{Kind: Kind}, core.ActionPrincipal{}, "opaque-token", core.LangEnglish); err == nil {
		t.Fatal("tool mode accepted the legacy begin path")
	}
	if err := a.EnableTools(); err == nil {
		t.Fatal("mode changed after workspace initialization")
	}
}

func TestLegacyModeDoesNotImplicitlyEnableToolEndpoint(t *testing.T) {
	a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		t.Fatal("legacy adapter invoked a tool helper")
		return nil, nil
	}}
	data, card, err := a.Tool(context.Background(), "customer", json.RawMessage(`{"customer_query":"C-001"}`), core.ActionPrincipal{}, "opaque-token", core.LangEnglish)
	if err != nil || data["status"] != "blocked" || card != nil {
		t.Fatalf("legacy mode did not hold tool endpoint: %v / %v", data, err)
	}
}

func TestToolReturnsCanonicalFactsAndHostCardWithoutRequestFile(t *testing.T) {
	principal := core.ActionPrincipal{Platform: "mock", UserID: "sender-1", ChatID: "group-1", SessionKey: "mock:group-1:sender-1", Project: "test", MessageID: "message-1"}
	a := &Adapter{command: "fixed-helper", hostSecret: strings.Repeat("s", 32), run: func(_ context.Context, _, command string, input []byte, env []string) ([]byte, error) {
		if command != "host-tool" || !envHas(env, actionTokenEnv, strings.Repeat("t", 32)) {
			t.Fatal("tool did not use the authenticated host protocol")
		}
		var request struct {
			Command   string               `json:"command"`
			Request   map[string]any       `json:"request"`
			Principal core.ActionPrincipal `json:"principal"`
		}
		if err := json.Unmarshal(input, &request); err != nil {
			t.Fatal(err)
		}
		if request.Command != "stage" || request.Principal != principal || request.Request["content"] != "Fictional follow-up" {
			t.Fatalf("host request = %#v", request)
		}
		return []byte(`{"status":"pending","change_id":"chg_Abcdefgh12345678","approval_id":"apr_Abcdefgh12345678","preview":{"actor":"Fixture Owner","customer":{"name":"<CRM_DATA>Acme</CRM_DATA>"},"effects":{"followup":{"after":{"content":"<CRM_DATA>Fictional follow-up</CRM_DATA>"}}}}}`), nil
	}}
	if err := a.EnableTools(); err != nil {
		t.Fatal(err)
	}
	data, card, err := a.Tool(context.Background(), "stage", json.RawMessage(`{"customer_query":"C-001","content":"Fictional follow-up"}`), principal, strings.Repeat("t", 32), core.LangEnglish)
	if err != nil {
		t.Fatal(err)
	}
	if data["preview"] == nil || card == nil || card.Card == nil || !strings.Contains(card.Card.RenderText(), "Fictional follow-up") || card.ChangeID != data["change_id"] {
		t.Fatalf("missing model facts or host card: %v / %#v", data, card)
	}
}

func TestToolRefusesWritesAndMissingSessionBeforeHelper(t *testing.T) {
	a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		t.Fatal("unexpected helper call")
		return nil, nil
	}}
	if err := a.EnableTools(); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"approve", "apply", "host-execute", "raw-api", "stage"} {
		token := strings.Repeat("t", 32)
		if command == "stage" {
			token = ""
		}
		data, card, err := a.Tool(context.Background(), command, json.RawMessage(`{}`), core.ActionPrincipal{}, token, core.LangEnglish)
		if err != nil || data["status"] != "blocked" || card != nil {
			t.Fatalf("%s was not held safely", command)
		}
	}
}

func TestOpenToolReturnsHostLinkWithoutApprovalCard(t *testing.T) {
	principal := core.ActionPrincipal{UserID: "sender", ChatID: "chat", Project: "fixed"}
	a := &Adapter{toolsEnabled: true, hostSecret: strings.Repeat("s", 32), run: func(_ context.Context, _, command string, input []byte, _ []string) ([]byte, error) {
		var payload struct {
			Command   string               `json:"command"`
			Request   map[string]any       `json:"request"`
			Principal core.ActionPrincipal `json:"principal"`
		}
		if err := json.Unmarshal(input, &payload); err != nil {
			t.Fatal(err)
		}
		if command != "host-tool" || payload.Command != "open" || len(payload.Request) != 0 || payload.Principal != principal {
			t.Fatalf("unexpected host request: %s", input)
		}
		return []byte(`{"status":"available","url":"https://example.invalid/base/fixture"}`), nil
	}}
	data, card, err := a.Tool(context.Background(), "open", json.RawMessage(`{}`), principal, strings.Repeat("t", 32), core.LangEnglish)
	if err != nil || card != nil || data["url"] != "https://example.invalid/base/fixture" {
		t.Fatalf("open did not return a read-only link: %v / %v", data, err)
	}
}
