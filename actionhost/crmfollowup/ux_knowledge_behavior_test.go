package crmfollowup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/teambrain"
	"github.com/chenhg5/cc-connect/core"
)

type observedUXKnowledge struct {
	*teambrain.Adapter
	observe func(realCanaryObservation)
}

func (a *observedUXKnowledge) Tool(ctx context.Context, command string, raw json.RawMessage, p core.ActionPrincipal, token string, lang core.Language) (map[string]any, *core.ActionHostResult, error) {
	data, card, err := a.Adapter.Tool(ctx, command, raw, p, token, lang)
	var input map[string]any
	if json.Unmarshal(raw, &input) == nil {
		a.observe(realCanaryObservation{command, input, data})
	}
	return data, card, err
}

// One three-turn real Engine journey covers paginated hit -> body/source answer
// -> a different subject with no hits -> explaining that bounded absence.
// Exact semantic wording is reviewed from platform evidence, not string-scored.
func runUXKnowledgeCase(t *testing.T, scratch, policy string, turn func(string) []realCanaryObservation, p *toolJourneyPlatform) {
	t.Helper()
	var evidence []map[string]any
	t.Cleanup(func() {
		data, err := json.MarshalIndent(map[string]any{
			"case": "knowledge-query", "policy_sha256": policy,
			"model": os.Getenv("MYANC_REAL_CLAUDE_MODEL"), "trial": os.Getenv("MYANC_REAL_CLAUDE_TRIAL"),
			"turns": evidence, "code_grader_passed": !t.Failed(),
			"semantic_review": "pending: distinguish accounts from people, Excel from API, read source from title, and query absence from global absence",
		}, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(scratch, "ux-evidence.json"), data, 0600)
		}
		if err != nil {
			t.Error(err)
		}
	})
	observe := func(message, subject string) ([]realCanaryObservation, string) {
		t.Helper()
		calls, shown := captureNativeTurn(p, message, turn, &evidence)
		for _, c := range calls {
			if (c.command != "knowledge_search" && c.command != "knowledge_read") || c.input["subject"] != subject {
				t.Fatalf("unrequested command or changed customer scope: %s", c.command)
			}
			if c.data["status"] != "ok" {
				t.Fatal("knowledge tool did not complete")
			}
		}
		return calls, text(shown["reply"])
	}
	calls, reply := observe("查一下团队知识里客户标识「演示·青松B」的系统情况：有多少账号，能不能接 API？给我资料出处。只查询，不新增或修改。", "演示·青松B")
	continued, read := false, false
	var firstQuery any
	for _, c := range calls {
		if c.command == "knowledge_search" {
			if firstQuery == nil {
				firstQuery = c.input["query"]
			}
			if c.input["page_token"] == "synthetic-next" && c.input["query"] == firstQuery {
				continued = true
			}
		}
		if c.command == "knowledge_read" && continued {
			source, _ := c.data["source"].(map[string]any)
			read = source["node_token"] == "syntheticqingsong"
		}
	}
	if !continued || !read || !strings.Contains(reply, "https://synthetic.feishu.cn/wiki/syntheticqingsong") {
		t.Fatal("answer did not continue filtered page, read source and show its URL")
	}
	calls, _ = observe("再查客户标识「演示·白鹭C」有没有类似资料，也只查询。", "演示·白鹭C")
	if len(calls) == 0 {
		t.Fatal("zero-hit task skipped the actual search")
	}
	for _, c := range calls {
		if c.command != "knowledge_search" {
			t.Fatal("zero-hit query accessed another customer's body")
		}
		sources, ok := c.data["sources"].([]any)
		if !ok || len(sources) != 0 {
			t.Fatal("zero-hit fixture did not return an empty bounded result")
		}
	}
	calls, _ = observe("所以只是这次没搜到，不能确定整个知识库都没有，对吧？不用再查。", "演示·白鹭C")
	if len(calls) != 0 {
		t.Fatal("explanation requested without another operation")
	}
	if len(p.cardIDs) != 0 {
		t.Fatal("read-only knowledge journey generated an approval card")
	}
}

func TestKnowledgeCanaryFixtureSearchReadAndScope(t *testing.T) {
	root := os.Getenv("MYANC_TEAM_BRAIN_ROOT")
	if root == "" {
		t.Skip("explicit frozen knowledge repository required for cross-repository fixture test")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Python required when knowledge repository is supplied")
	}
	state := t.TempDir()
	call := func(command string, input map[string]any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"operation": "tool", "command": command, "input": input,
			"session_token": "synthetic-session", "principal": core.ActionPrincipal{Platform: "feishu", UserID: "sender-1", ChatID: "group-1", SessionKey: "feishu:group-1:sender-1", Project: "test"}})
		cmd := exec.CommandContext(context.Background(), python, "-B", "testdata/knowledge_fixture.py", "--root", root, "--state", state)
		cmd.Stdin = strings.NewReader(string(body))
		output, err := cmd.CombinedOutput()
		var data map[string]any
		if err != nil || json.Unmarshal(output, &data) != nil {
			t.Fatalf("knowledge fixture failed: %v: %s", err, output)
		}
		return data
	}
	first := call("knowledge_search", map[string]any{"query": "青松", "subject": "演示·青松B"})
	if first["status"] != "ok" || len(first["sources"].([]any)) != 0 || first["next_page_token"] != "synthetic-next" {
		t.Fatal("expected filtered first page with continuation")
	}
	second := call("knowledge_search", map[string]any{"query": "青松", "subject": "演示·青松B", "page_token": first["next_page_token"]})
	if len(second["sources"].([]any)) != 1 {
		t.Fatal("expected source on next page")
	}
	read := call("knowledge_read", map[string]any{"node_token": "syntheticqingsong", "subject": "演示·青松B"})
	if read["status"] != "ok" || !strings.Contains(text(read["source"].(map[string]any)["text"]), "账号约 20 个") {
		t.Fatal("real knowledge host did not return source body")
	}
	if call("knowledge_read", map[string]any{"node_token": "syntheticqingsong", "subject": "演示·白鹭C"})["code"] != "subject_mismatch" {
		t.Fatal("knowledge scope enforcement missing")
	}
	missing := call("knowledge_search", map[string]any{"query": "白鹭", "subject": "演示·白鹭C"})
	if missing["status"] != "ok" || len(missing["sources"].([]any)) != 0 || missing["next_page_token"] != "" {
		t.Fatal("missing source must be bounded empty search")
	}
}
