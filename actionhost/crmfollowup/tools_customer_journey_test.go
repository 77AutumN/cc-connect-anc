package crmfollowup

// Real Engine + authenticated HTTP + adapter subprocess + Python SQLite;
// only the model, platform transport and Feishu backend are scripted fixtures.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCUJ_CRMIPC2_ProfileThenIndependentFollowup(t *testing.T) {
	root := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if root == "" {
		t.Skip("set MYANC_SPIKE_CRM_ROOT for the offline cross-repo journey")
	}
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	var executes atomic.Int32
	a := &Adapter{hostSecret: "offline-gateway-host-secret-00000000000001", toolsEnabled: true}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkDir(scratch, account.Username); err != nil {
		t.Fatal(err)
	}
	a.run = func(ctx context.Context, _ string, subcommand string, input []byte, env []string) ([]byte, error) {
		if subcommand == "host-execute" {
			executes.Add(1)
		}
		cmd := exec.CommandContext(ctx, python, "-B", filepath.Join(root, "spikes", "host_tool_fixture.py"), subcommand)
		cmd.Env = append(env, "MYANC_SPIKE_DIR="+scratch, "MYANC_SPIKE_SEED=customer-trial")
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.Output()
		if err != nil {
			if failure, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("offline fixture: %w: %s", err, failure.Stderr)
			}
		}
		return output, err
	}
	p := &toolJourneyPlatform{cards: make(map[string]*core.Card)}
	agent := &toolJourneyAgent{client: &http.Client{Timeout: 15 * time.Second}, steps: make(chan toolJourneyStep, 1), results: make(chan toolJourneyReply, 1), events: make(chan core.Event, 16)}
	e := core.NewEngine("test", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	e.SetActionHost(a)
	server := httptest.NewServer(e.ActionToolHandler())
	agent.url = server.URL
	t.Cleanup(server.Close)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	const key = "mock:group-1:sender-1"
	n := 0
	receive := func(content string) {
		e.ReceiveMessage(p, &core.Message{Platform: "mock", SessionKey: key, UserID: "sender-1", ChannelID: "group-1", MessageID: fmt.Sprintf("inbound-%d", n), Content: content, ReplyCtx: "fixture-group"})
	}
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !strings.Contains(p.transcript(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("missing %q: %s", want, p.transcript())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	receive("/quiet quiet")
	wait("Quiet mode")
	turn := func(command string, input map[string]any, status string) map[string]any {
		t.Helper()
		n++
		agent.steps <- toolJourneyStep{number: n, command: command, input: input, plain: "Read-only discussion"}
		receive("Please use " + command + " for this fictional customer, or continue discussing")
		var reply toolJourneyReply
		select {
		case reply = <-agent.results:
		case <-time.After(15 * time.Second):
			t.Fatal("tool turn did not finish")
		}
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		wait(fmt.Sprintf("TURN-%d ", n))
		if status != "" && reply.data["status"] != status {
			t.Fatalf("%s wanted %s: %v", command, status, reply.data)
		}
		return reply.data
	}
	click := func(plan map[string]any, cardID, user string, decision core.ActionDecision) core.TrustedCardActionResponse {
		t.Helper()
		response := p.callback(core.TrustedCardAction{Kind: Kind, ApprovalID: text(plan["approval_id"]), Decision: decision, Language: core.LangEnglish, Principal: core.ActionPrincipal{UserID: user, ChatID: "group-1", SessionKey: key, MessageID: cardID}})
		if response.Card != nil {
			if err := p.RefreshCardMessage(context.Background(), cardID, key, response.Card); err != nil {
				t.Fatal(err)
			}
		}
		return response
	}
	finish := func(plan map[string]any, cardID string) {
		t.Helper()
		before := agent.turns.Load()
		claimed := click(plan, cardID, "sender-1", core.ActionApprove)
		if claimed.Complete == nil {
			t.Fatalf("approval not claimed: %+v", claimed)
		}
		completed := claimed.Complete()
		if completed.Card == nil || completed.Card.Header.Color != "green" {
			t.Fatalf("parent receipt not verified: %+v", completed)
		}
		if err := p.RefreshCardMessage(context.Background(), cardID, key, completed.Card); err != nil {
			t.Fatal(err)
		}
		if agent.turns.Load() != before {
			t.Fatal("approval resumed Claude")
		}
	}
	turn("customer", map[string]any{"customer_query": "C-001"}, "found")
	turn("stage-customer-create", map[string]any{"name": "虚构公司"}, "needs_input")
	person := turn("assignee", map[string]any{"query": "member@example.com"}, "resolved")
	parent := turn("stage-customer-create", map[string]any{"name": "虚构评测公司", "contact": "示例甲", "stage": "新线索", "owner_ref": person["owner_ref"], "followup": map[string]any{"occurred_at": "2026-09-06T10:00:00+08:00", "content": "明确要求演示", "next_action": "准备演示"}}, "pending")
	parentCard, _ := p.lastCard()
	for _, user := range []string{"sender-2"} {
		denied := click(parent, parentCard, user, core.ActionApprove)
		if denied.ToastType != "error" || denied.Complete != nil {
			t.Fatal("wrong sender accepted")
		}
	}
	finish(parent, parentCard)
	childCard, childView := p.lastCard()
	if childCard == parentCard || !strings.Contains(childView.RenderText(), "preceding customer operation is complete") {
		t.Fatal("separate bound follow-up card missing")
	}
	var childID string
	for _, element := range childView.Elements {
		if actions, ok := element.(core.CardActions); ok {
			childID = actions.Buttons[0].Extra["approval_id"]
		}
	}
	child := map[string]any{"approval_id": childID}
	// Exact message binding: a genuine owner cannot use the parent card ID to
	// authorize the second action, nor can a duplicate parent republish it.
	wrongCard := click(child, parentCard, "sender-1", core.ActionApprove)
	if wrongCard.ToastType != "error" || wrongCard.Complete != nil {
		t.Fatalf("parent card response for child %q: %+v", childID, wrongCard)
	}
	replay := click(parent, parentCard, "sender-1", core.ActionApprove)
	if replay.Complete != nil {
		t.Fatal("parent replay scheduled execution")
	}
	p.mu.Lock()
	count := len(p.cardIDs)
	p.mu.Unlock()
	if count != 2 || executes.Load() != 1 {
		t.Fatalf("parent replay created a card/write: cards=%d writes=%d", count, executes.Load())
	}
	finish(child, childCard)
	parentResult := turn("result", map[string]any{"change_id": parent["change_id"]}, "verified")
	related := parentResult["related_operations"].(map[string]any)
	if related["child"].(map[string]any)["status"] != "verified" {
		t.Fatal("child result not linked")
	}
	// Update an existing customer, then cancel its independently staged follow-up.
	updated := turn("stage-customer-update", map[string]any{"customer_query": "C-001", "changes": map[string]any{"stage": "沟通中"}, "followup": map[string]any{"occurred_at": "2026-09-06T11:00:00+08:00", "content": "演示完成"}}, "pending")
	updateCard, _ := p.lastCard()
	finish(updated, updateCard)
	cancelCard, cancelView := p.lastCard()
	for _, element := range cancelView.Elements {
		if actions, ok := element.(core.CardActions); ok {
			childID = actions.Buttons[0].Extra["approval_id"]
		}
	}
	cancelled := click(map[string]any{"approval_id": childID}, cancelCard, "sender-1", core.ActionCancel)
	if cancelled.Complete != nil || !strings.Contains(cancelled.Card.RenderText(), "customer operation completed") {
		t.Fatal("child cancellation hid parent success")
	}
	turn("result", map[string]any{"change_id": updated["change_id"]}, "verified")
	facts := turn("customer", map[string]any{"customer_query": "C-001"}, "found")
	factJSON, _ := json.Marshal(facts)
	historyJSON, _ := json.Marshal(facts["history"])
	if !bytes.Contains(factJSON, []byte("沟通中")) || bytes.Contains(historyJSON, []byte("演示完成")) {
		t.Fatalf("cancelled follow-up changed facts: %s", factJSON)
	}
	turn("", nil, "")
	if executes.Load() != 3 || agent.starts.Load() != 1 || agent.permissions.Load() != 0 || agent.closes.Load() != 0 || strings.Contains(p.transcript(), "INTERNAL_SPIKE_") {
		t.Fatal("host replay/Quiet/continued-session invariant failed")
	}
}
