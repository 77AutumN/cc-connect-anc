package crmfollowup

// Real Engine/session/HTTP and Python Brain/SQLite; scripted model, fake Feishu,
// and a portable subprocess launcher instead of the production POSIX wrapper.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/actionhost/teambrain"
	"github.com/chenhg5/cc-connect/core"
)

type catalogFixtureHost struct {
	*teambrain.Adapter
	python, root, state string
}

func (*catalogFixtureHost) Enabled(project string) bool { return project == "test" }
func (h *catalogFixtureHost) Tool(ctx context.Context, command string, input json.RawMessage, p core.ActionPrincipal, token string, _ core.Language) (map[string]any, *core.ActionHostResult, error) {
	body, _ := json.Marshal(map[string]any{"operation": "tool", "command": command, "input": input, "principal": p, "session_token": token})
	cmd := exec.CommandContext(ctx, h.python, "-B", "testdata/knowledge_fixture.py", "--root", h.root, "--state", h.state)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	cmd.Stdin = bytes.NewReader(body)
	output, err := cmd.Output()
	var result map[string]any
	if err == nil {
		err = json.Unmarshal(output, &result)
	}
	return result, nil, err
}

func TestCUJ_KNOWLEDGEIPC2_CatalogReadFollowupAndScope(t *testing.T) {
	root := os.Getenv("MYANC_TEAM_BRAIN_ROOT")
	if root == "" {
		t.Skip("explicit frozen knowledge repository required")
	}
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	crm := &Adapter{toolsEnabled: true}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := crm.SetWorkDir(state, account.Username); err != nil {
		t.Fatal(err)
	}
	knowledge := &catalogFixtureHost{python: python, root: root, state: state}
	p := &reminderJourneyPlatform{toolJourneyPlatform: toolJourneyPlatform{cards: map[string]*core.Card{}}}
	a := &toolJourneyAgent{client: &http.Client{Timeout: 10 * time.Second}, steps: make(chan toolJourneyStep, 1), results: make(chan toolJourneyReply, 1), events: make(chan core.Event, 16)}
	e := core.NewEngine("test", a, []core.Platform{p}, filepath.Join(state, "sessions.json"), core.LangEnglish)
	e.SetActionHost(core.CombineActionHosts(crm, knowledge, teambrain.Commands()))
	server := httptest.NewServer(e.ActionToolHandler())
	defer server.Close()
	a.url = server.URL
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Stop() }()
	turn := 0
	send := func(message, command string, input map[string]any) map[string]any {
		t.Helper()
		turn++
		a.steps <- toolJourneyStep{number: turn, command: command, input: input, plain: message}
		e.ReceiveMessage(p, &core.Message{Platform: "feishu", SessionKey: "feishu:group-1:sender-1", UserID: "sender-1", ChannelID: "group-1", MessageID: fmt.Sprintf("catalog-%d", turn), Content: message, ReplyCtx: "group-1"})
		select {
		case result := <-a.results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			deadline := time.Now().Add(5 * time.Second)
			prefix := fmt.Sprintf("TURN-%d ", turn)
			for !strings.Contains(p.transcript(), prefix) {
				if time.Now().After(deadline) {
					t.Fatal("missing user-visible result")
				}
				time.Sleep(5 * time.Millisecond)
			}
			sent := p.transcript()
			var visible map[string]any
			if err := json.NewDecoder(strings.NewReader(sent[strings.Index(sent, prefix)+len(prefix):])).Decode(&visible); err != nil {
				t.Fatal("invalid user-visible knowledge result", err)
			}
			return visible
		case <-time.After(12 * time.Second):
			t.Fatal("knowledge tool timeout")
			return nil
		}
	}
	catalog := send("第一次拜访负责人，该问什么？", "knowledge_catalog", map[string]any{"subject": "team"})
	if catalog["status"] != "ok" || len(catalog["sources"].([]any)) != 1 {
		t.Fatal(catalog)
	}
	node := catalog["sources"].([]any)[0].(map[string]any)["node_token"]
	read := send("展开这份资料", "knowledge_read", map[string]any{"subject": "team", "node_token": node})
	if read["status"] != "ok" || !strings.Contains(text(read["source"].(map[string]any)["text"]), "返工或等待") {
		t.Fatal(read)
	}
	send("只解释引用的“明天提醒我”，不要执行", "", nil)
	blocked := send("尝试读取不属于该主题的客户页", "knowledge_read", map[string]any{"subject": "team", "node_token": "syntheticqingsong"})
	if blocked["code"] != "subject_mismatch" {
		t.Fatal("customer scope leaked", blocked)
	}
	if a.starts.Load() != 1 || len(p.cardIDs) != 0 || a.permissions.Load() != 0 {
		t.Fatal("read-only journey changed sessions or approval boundary")
	}
}
