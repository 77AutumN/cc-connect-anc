package crmfollowup

// Offline integration: real Engine, authenticated tool HTTP, adapter subprocess,
// CRM approval/SQLite/outbox; only agent intent, platform and Feishu are fakes.
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

func TestCUJ_CRMDaily1_ListAndPlanApproval(t *testing.T) {
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
	a := &Adapter{hostSecret: "offline-gateway-host-secret-00000000000001", toolsEnabled: true}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkDir(scratch, account.Username); err != nil {
		t.Fatal(err)
	}
	const privateRoute = "offline-private-sender-1"
	a.ConfigurePlanBinding(func(p core.ActionPrincipal) map[string]any {
		return map[string]any{"user_id": p.UserID, "source_chat_id": p.ChatID,
			"project": p.Project, "private_chat_id": privateRoute}
	})
	var executes atomic.Int32
	a.run = func(ctx context.Context, _ string, command string, input []byte, env []string) ([]byte, error) {
		if command == "host-execute" {
			executes.Add(1)
		}
		cmd := exec.CommandContext(ctx, python, "-B", filepath.Join(root, "spikes", "host_tool_fixture.py"), command)
		cmd.Env = append(env, "MYANC_SPIKE_DIR="+scratch, "MYANC_SPIKE_SEED=daily-followup")
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.Output()
		if failure, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("offline fixture: %w: %s", err, failure.Stderr)
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
		e.ReceiveMessage(p, &core.Message{Platform: "mock", SessionKey: key, UserID: "sender-1", ChannelID: "group-1", MessageID: fmt.Sprintf("daily-%d", n), Content: content, ReplyCtx: "fixture-group"})
	}
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !strings.Contains(p.transcript(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("missing visible response %q: %s", want, p.transcript())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	receive("/quiet quiet")
	wait("Quiet mode")
	turn := func(command string, input map[string]any, status string) map[string]any {
		t.Helper()
		n++
		agent.steps <- toolJourneyStep{number: n, command: command, input: input}
		receive("Use " + command + " for the fictional daily follow-up")
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
		if reply.data["status"] != status {
			t.Fatalf("%s wanted %s: %v", command, status, reply.data)
		}
		return reply.data
	}
	planCall := func(command string, input map[string]any) map[string]any {
		t.Helper()
		result, err := a.PlanCall(context.Background(), command, input)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	page := turn("customers", map[string]any{}, "found")
	rows := page["customers"].([]any)
	if len(rows) != 20 || page["has_more"] != true {
		t.Fatalf("default owner first page: %v", page)
	}
	secondID := "C-002" // ID read from the original list, not the next page's order.
	if !strings.Contains(text(rows[1].(map[string]any)["customer_number"]), secondID) {
		t.Fatalf("stable customer list order: %v", rows[1])
	}
	lastPage := turn("customers", page["next_request"].(map[string]any), "found")
	if len(lastPage["customers"].([]any)) != 1 || lastPage["has_more"] != false || lastPage["next_request"] != nil {
		t.Fatalf("owner filtering or final page failed: %v", lastPage)
	}
	history := turn("customer", map[string]any{"customer_query": secondID}, "found")
	if len(history["history"].([]any)) != 10 || history["has_more"] != true {
		t.Fatalf("history first page: %v", history)
	}
	older := turn("customer", map[string]any{"customer_query": secondID, "history_offset": history["next_offset"]}, "found")
	if len(older["history"].([]any)) != 2 || older["has_more"] != false {
		t.Fatalf("history tail page: %v", older)
	}
	const action = "Daily fictional approved plan"
	staged := turn("stage-customer-update", map[string]any{"customer_query": secondID, "changes": map[string]any{"next_action": action, "next_followup_at": "2099-02-01"}}, "pending")
	cardID, card := p.lastCard()
	if card == nil || !strings.Contains(card.RenderText(), action) || !strings.Contains(card.RenderText(), "2099-02-01") {
		t.Fatalf("plan preview missing in Quiet mode: %v", staged)
	}
	if pending := planCall("host-plan-events", map[string]any{}); len(pending["events"].([]any)) != 0 {
		t.Fatalf("unapproved plan reached outbox: %v", pending)
	}
	current := turn("customer", map[string]any{"customer_query": secondID}, "found")
	if strings.Contains(text(current["customer"].(map[string]any)["next_action"]), action) {
		t.Fatal("preview changed CRM before approval")
	}
	click := core.TrustedCardAction{Kind: Kind, ApprovalID: text(staged["approval_id"]), Decision: core.ActionApprove, Language: core.LangEnglish,
		Principal: core.ActionPrincipal{UserID: "sender-2", ChatID: "group-1", SessionKey: key, MessageID: cardID}}
	if denied := p.callback(click); denied.ToastType != "error" || denied.Complete != nil {
		t.Fatal("another sender approved the plan")
	}
	click.Principal.UserID = "sender-1"
	approved := p.callback(click)
	if approved.Complete == nil {
		t.Fatalf("plan approval not claimed: %+v", approved)
	}
	completed := approved.Complete()
	if completed.Card == nil || completed.Card.Header.Color != "green" {
		t.Fatalf("plan not verified: %+v", completed)
	}
	if err := p.RefreshCardMessage(context.Background(), cardID, key, completed.Card); err != nil {
		t.Fatal(err)
	}
	verified := turn("result", map[string]any{"change_id": staged["change_id"]}, "verified")
	if receipt := verified["receipt"].(map[string]any); receipt["plan_reminder_status"] != "pending_sync" {
		t.Fatalf("CRM success wrongly claims reminder success: %v", receipt)
	}
	events := planCall("host-plan-events", map[string]any{})["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("verified plan must create exactly one durable outbox event: %v", events)
	}
	event := events[0].(map[string]any)
	binding := event["reminder_binding"].(map[string]any)
	if event["customer_id"] != "rec-2" || event["revision"] != float64(1) || event["content"] != action || event["at"] != "2099-02-01T09:00:00+08:00" ||
		binding["private_chat_id"] != privateRoute || binding["user_id"] != "sender-1" || binding["source_chat_id"] != "group-1" || binding["project"] != "test" {
		t.Fatalf("event lost the exact customer, revision or frozen host route: %v", event)
	}
	if replay := p.callback(click); replay.Complete != nil || executes.Load() != 1 {
		t.Fatal("duplicate approval re-executed")
	}
	if again := planCall("host-plan-events", map[string]any{}); len(again["events"].([]any)) != 1 {
		t.Fatalf("duplicate approval duplicated the outbox: %v", again)
	}
	read := planCall("host-plan-read", map[string]any{"namespace": event["namespace"], "customer_id": event["customer_id"], "revision": event["revision"]})
	if read["status"] != "current" {
		t.Fatalf("host could not validate exact current plan: %v", read)
	}
	ack := planCall("host-plan-ack", map[string]any{"event_id": event["event_id"]})
	if ack["status"] != "acknowledged" || len(planCall("host-plan-events", map[string]any{})["events"].([]any)) != 0 {
		t.Fatal("outbox acknowledgement did not survive a new process")
	}
	after := turn("customer", map[string]any{"customer_query": secondID}, "found")
	if !strings.Contains(text(after["customer"].(map[string]any)["next_action"]), action) || len(after["history"].([]any)) != 10 {
		t.Fatalf("approved plan or communication history changed unexpectedly: %v", after)
	}
	data, err := os.ReadFile(filepath.Join(scratch, "fake-feishu.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Actions   []string                   `json:"actions"`
		Followups map[string]json.RawMessage `json:"followups"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Actions) != 1 || stored.Actions[0] != "update_customer_profile" || len(stored.Followups) != 12 {
		t.Fatalf("independent plan fabricated communication or duplicated write: %+v", stored)
	}
	if strings.Contains(p.transcript(), privateRoute) || strings.Contains(p.transcript(), "INTERNAL_SPIKE_") || !strings.Contains(p.transcript(), action) || agent.starts.Load() != 1 || agent.permissions.Load() != 0 {
		t.Fatal("visible conversation leaked host state or lost normal session continuity")
	}
}
