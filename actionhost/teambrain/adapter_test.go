package teambrain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestDisabledHostAndSessionEnvironmentExposeNoPrivateConfiguration(t *testing.T) {
	t.Setenv("CC_TEAM_BRAIN_COMMAND", "")
	t.Setenv("CC_TEAM_BRAIN_PROJECTS", "")
	a, err := NewFromEnv()
	if err != nil || a != nil {
		t.Fatalf("disabled host = %v %v", a, err)
	}
	a = &Adapter{command: "private-host-path", projects: map[string]bool{"project": true}}
	env, err := a.SessionEnv("session")
	if err != nil || len(env) != 1 || env[0] != "TEAM_BRAIN_SESSION_TOKEN=session" {
		t.Fatal("private config in model env")
	}
	for _, command := range []string{"approve", "execute", "raw-api"} {
		result, card, err := a.Tool(context.Background(), command, json.RawMessage(`{}`), core.ActionPrincipal{}, "session", core.LangEnglish)
		if err != nil || card != nil || result["status"] != "blocked" {
			t.Fatal("model obtained privileged operation")
		}
	}
}

func TestSupervisorRestartPreservesKnowledgeConfigurationWithoutDuplicateKeys(t *testing.T) {
	a := &Adapter{command: "/protected/host", projects: map[string]bool{"second": true, "first": true}}
	env := a.RestartEnv([]string{"PATH=/bin", "CC_TEAM_BRAIN_COMMAND=wrong", "CC_TEAM_BRAIN_PROJECTS=wrong"})
	if len(env) != 3 || env[0] != "PATH=/bin" || env[1] != "CC_TEAM_BRAIN_COMMAND=/protected/host" || env[2] != "CC_TEAM_BRAIN_PROJECTS=first,second" {
		t.Fatalf("invalid supervisor environment: %v", env)
	}
	model, err := a.SessionEnv("session")
	if err != nil || len(model) != 1 || strings.Contains(model[0], "protected") {
		t.Fatal("restart config leaked to model")
	}
}

func TestPresentationHasCompletePreviewAndTrustedButtonsInFiveLanguages(t *testing.T) {
	for _, lang := range []core.Language{core.LangEnglish, core.LangChinese, core.LangTraditionalChinese, core.LangJapanese, core.LangSpanish} {
		result := present(map[string]any{"status": "pending", "approval_id": "bound-id", "preview": "Title: page one\nTarget: https://synthetic.feishu.cn/wiki/page1\nPrior: Old context\n- Old\n+ New ``` untrusted"}, lang)
		if !strings.Contains(result.Card.RenderText(), "Old") || !strings.Contains(result.Card.RenderText(), "New") {
			t.Fatal("preview lost changes")
		}
		if !strings.Contains(result.Card.RenderText(), "page1") || !strings.Contains(result.Card.RenderText(), "Old context") {
			t.Fatal("lost destination or prior context")
		}
		count := 0
		for _, element := range result.Card.Elements {
			if actions, ok := element.(core.CardActions); ok {
				for _, button := range actions.Buttons {
					count++
					if button.Extra["kind"] != Kind || button.Extra["approval_id"] != "bound-id" || button.Text == "" {
						t.Fatal("unbound button")
					}
				}
			}
		}
		if count != 3 {
			t.Fatal("missing approval choices")
		}
	}
	unknown := present(map[string]any{"status": "unknown"}, core.LangChinese)
	if unknown.Code != "execution_result_unknown" || unknown.Card.HasButtons() {
		t.Fatal("unknown write allowed replay")
	}
}

func TestSubprocessReceivesAuthenticatedPrincipalAndNoInheritedSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production adapter is POSIX only")
	}
	directory := t.TempDir()
	capture := filepath.Join(directory, "request.json")
	script := filepath.Join(directory, "host")
	body := "#!/bin/sh\ncat > '" + capture + "'\nif [ -n \"$SYNTHETIC_SECRET\" ]; then exit 1; fi\nprintf '%s' '{\"status\":\"ok\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_SECRET", "must-not-forward")
	a := &Adapter{command: script, projects: map[string]bool{"project": true}}
	principal := core.ActionPrincipal{UserID: "member", ChatID: "chat", SessionKey: "session", Project: "project", Platform: "test"}
	result, _, err := a.Tool(context.Background(), "knowledge_search", json.RawMessage(`{"query":"synthetic","subject":"team"}`), principal, "scoped-token", core.LangEnglish)
	if err != nil || result["status"] != "ok" {
		t.Fatalf("host call %v %v", result, err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if json.Unmarshal(raw, &request) != nil {
		t.Fatal("invalid envelope")
	}
	actor := request["principal"].(map[string]any)
	if actor["user_id"] != "member" || actor["project"] != "project" || request["operation"] != "tool" || request["session_token"] != "scoped-token" {
		t.Fatal("lost trusted context")
	}
}
