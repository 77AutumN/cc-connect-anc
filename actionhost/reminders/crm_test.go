package reminders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/actionhost/opsalerts"
	"github.com/chenhg5/cc-connect/core"
)

func planFixture(h *Host, project string, revision int) CRMPlan {
	p := principal(project, "approved")
	r := h.routes[project]
	return CRMPlan{EventID: fmt.Sprintf("event-%d", revision), Namespace: "fixture-crm", CustomerID: "customer-1", Revision: revision, ChangeID: fmt.Sprintf("change-%d", revision), Principal: p, At: h.now().Add(time.Hour).Format(time.RFC3339), Content: "Prepare fictional follow-up", Enabled: true, CustomerQuery: "C-001", CustomerName: "Fictional customer", Binding: CRMRoute{r.User, r.Chat, r.Project, r.PrivateChat}}
}

func planStore(t *testing.T, h *Host, fn func(*state)) {
	t.Helper()
	if err := h.store.change(context.Background(), func(st *state) error { fn(st); return nil }); err != nil {
		t.Fatal(err)
	}
}

func applyPlan(t *testing.T, h *Host, p CRMPlan) {
	t.Helper()
	if err := h.applyCRMPlan(context.Background(), p, core.LangChinese); err != nil {
		t.Fatal(err)
	}
}

func currentBridge(h *Host, current *CRMPlan) {
	h.EnableCRM(func(_ context.Context, _, command string, _ map[string]any) (map[string]any, error) {
		switch command {
		case "host-plan-read":
			return map[string]any{"status": "current", "plan": *current}, nil
		case "host-plan-events":
			return map[string]any{"status": "ok", "events": []any{}}, nil
		default:
			return map[string]any{"status": "acknowledged"}, nil
		}
	}, nil)
}

func TestCRMPlanRejectsMissingOrForgedBindings(t *testing.T) {
	p := CRMPlan{EventID: "event", Namespace: "sandbox", CustomerID: "record", Revision: 1, ChangeID: "change", Principal: principal("ap", "message"), At: "2026-09-20T09:00:00+08:00", Content: "follow up", CustomerQuery: "C-1", Enabled: true, Binding: CRMRoute{"a", "pa", "ap", "pa"}}
	if !validCRMPlan(p) {
		t.Fatal("valid plan rejected")
	}
	for _, mutate := range []func(*CRMPlan){func(p *CRMPlan) { p.Binding.User = "b" }, func(p *CRMPlan) { p.Binding.Private = "" }, func(p *CRMPlan) { p.Namespace = "" }, func(p *CRMPlan) { p.Revision = 0 }, func(p *CRMPlan) { p.At = "tomorrow" }} {
		bad := p
		mutate(&bad)
		if validCRMPlan(bad) {
			t.Fatal("invalid plan accepted", bad)
		}
	}
}

func TestCRMPlanOutOfOrderReplacementCancellationAndRestart(t *testing.T) {
	h, _, _, path := fixture(t)
	manual := item(create(t, h, "ap", "manual", "ordinary personal reminder"))["id"].(string)
	one := planFixture(h, "ag", 1)
	applyPlan(t, h, one)
	var firstID string
	planStore(t, h, func(st *state) { firstID = st.Plans[one.key()].ReminderID })
	two := planFixture(h, "bg", 2)
	applyPlan(t, h, two)
	applyPlan(t, h, one)
	planStore(t, h, func(st *state) {
		if st.Items[firstID].Status != "cancelled" || st.Plans[one.key()].Plan.Revision != 2 || st.Items[manual].Status != "pending" {
			t.Fatal("old event or cross-person replacement broke isolation")
		}
	})
	three := planFixture(h, "ap", 3) // A -> B -> A, with exactly the same date.
	applyPlan(t, h, three)
	var id string
	planStore(t, h, func(st *state) { id = st.Plans[three.key()].ReminderID })
	if result := call(t, h, "reminder-cancel", "ap", "cancel", map[string]any{"id": id, "version": 1}); result["status"] != "cancelled" {
		t.Fatal(result)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h.store = store
	t.Cleanup(func() { _ = store.Close() })
	applyPlan(t, h, three)
	applyPlan(t, h, two)
	planStore(t, h, func(st *state) {
		if !st.Plans[three.key()].Disabled || st.Items[id].Status != "cancelled" || len(st.Items) != 4 {
			t.Fatal("replay revived notification")
		}
	})
	four := planFixture(h, "bp", 4)
	applyPlan(t, h, four)
	other := planFixture(h, "ap", 1)
	other.Namespace = "another-crm"
	applyPlan(t, h, other)
	planStore(t, h, func(st *state) {
		if st.Plans[four.key()].Disabled || st.Plans[other.key()].Plan.Revision != 1 || st.Items[manual].Status != "pending" {
			t.Fatal("new plan or namespace isolation failed")
		}
	})
}

func TestCRMOutboxAckOnlyAfterCommitAndReplayPreservesCancel(t *testing.T) {
	h, _, _, _ := fixture(t)
	plan := planFixture(h, "ap", 1)
	ackFails := true
	acks := 0
	h.EnableCRM(func(_ context.Context, _, command string, _ map[string]any) (map[string]any, error) {
		if command == "host-plan-events" {
			return map[string]any{"status": "ok", "events": []any{plan}}, nil
		}
		if command == "host-plan-ack" {
			acks++
			planStore(t, h, func(st *state) {
				if st.Plans[plan.key()] == nil {
					t.Fatal("ack before store commit")
				}
			})
			if ackFails {
				return nil, errors.New("fixture interrupted ack")
			}
			return map[string]any{"status": "acknowledged"}, nil
		}
		return nil, errors.New("unexpected read")
	}, nil)
	h.syncCRM(context.Background(), core.LangChinese)
	var id string
	planStore(t, h, func(st *state) { id = st.Plans[plan.key()].ReminderID })
	call(t, h, "reminder-cancel", "ap", "cancel", map[string]any{"id": id, "version": 1})
	ackFails = false
	h.syncCRM(context.Background(), core.LangChinese)
	planStore(t, h, func(st *state) {
		if len(st.Items) != 1 || !st.Plans[plan.key()].Disabled {
			t.Fatal("outbox retry recreated canceled notification")
		}
	})
	if acks != 2 {
		t.Fatal("missing ack retry")
	}
}

func TestCRMReadFailureDoesNotHoldOrdinaryReminder(t *testing.T) {
	h, sender, now, _ := fixture(t)
	plan := planFixture(h, "ap", 1)
	applyPlan(t, h, plan)
	currentBridge(h, &plan)
	callCRM := h.crmCall
	h.crmCall = func(ctx context.Context, project, command string, input map[string]any) (map[string]any, error) {
		if command == "host-plan-read" {
			return nil, errors.New("CRM read unavailable")
		}
		return callCRM(ctx, project, command, input)
	}
	create(t, h, "ap", "ordinary", "NORMAL-ONLY")
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 1 || sender.calls[0].chat != "pa" {
		t.Fatal("ordinary delivery was held by CRM")
	}
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 1 {
		t.Fatal("unchecked CRM reminder sent")
	}
	planStore(t, h, func(st *state) {
		if st.Items[st.Plans[plan.key()].ReminderID].Status != "paused" {
			t.Fatal("failed read did not pause")
		}
	})
	if snapshot := h.HealthSnapshot(context.Background()); len(snapshot.Faults) != 1 || snapshot.Faults[0].Category != opsalerts.Delivery {
		t.Fatal("CRM verification pause was missing or mislabeled as a permission failure", snapshot)
	}
	listed := call(t, h, "reminder-list", "ap", "paused-list", map[string]any{})
	listJSON, _ := json.Marshal(listed)
	if !strings.Contains(string(listJSON), "crm_plan_unverified") || !strings.Contains(string(listJSON), "请重新确认 CRM 计划") || strings.Contains(string(listJSON), plan.Namespace) {
		t.Fatal("paused reminder list has no safe recovery guidance", string(listJSON))
	}
	planStore(t, h, func(st *state) {
		current := st.Items[st.Plans[plan.key()].ReminderID]
		listState := emptyState()
		h.queueList(&listState, h.routes["ap"], []*Reminder{current}, core.LangChinese)
		for _, batch := range listState.Batches {
			if !strings.Contains(batch.Text, "请重新确认 CRM 计划") || strings.Contains(batch.Text, plan.Namespace) {
				t.Fatal("private queued list lost safe recovery guidance")
			}
		}
	})
}

func TestCRMEveryRetryRechecksPlanAndIdentity(t *testing.T) {
	for _, reason := range []string{"revision", "fields", "private-route", "source-revoked", "disabled"} {
		t.Run(reason, func(t *testing.T) {
			h, sender, now, _ := fixture(t)
			plan := planFixture(h, "ag", 1)
			applyPlan(t, h, plan)
			currentBridge(h, &plan)
			sender.err = errors.New("send acceptance unknown")
			*now = now.Add(time.Hour)
			if err := h.Tick(context.Background(), core.LangChinese); err != nil {
				t.Fatal(err)
			}
			if len(sender.calls) != 1 {
				t.Fatal("first attempt missing")
			}
			switch reason {
			case "revision":
				plan.Revision++
			case "fields":
				plan.Content = "manual Base change"
			case "private-route":
				route := h.routes["ag"]
				route.PrivateChat = "different-private"
				h.routes["ag"] = route
			case "source-revoked":
				h.sourceAuthorized = func(string, string) bool { return false }
			case "disabled":
				h.crmCall = nil
			}
			*now = now.Add(time.Minute)
			sender.err = nil
			if err := h.Tick(context.Background(), core.LangChinese); err != nil {
				t.Fatal(err)
			}
			if len(sender.calls) != 1 {
				t.Fatal("stale or revoked retry sent")
			}
		})
	}
}

func TestCRMCancelDuringReadPreventsSend(t *testing.T) {
	h, sender, now, _ := fixture(t)
	plan := planFixture(h, "ap", 1)
	applyPlan(t, h, plan)
	currentBridge(h, &plan)
	var id string
	planStore(t, h, func(st *state) { id = st.Plans[plan.key()].ReminderID })
	read := h.crmCall
	h.crmCall = func(ctx context.Context, project, command string, input map[string]any) (map[string]any, error) {
		if command == "host-plan-read" {
			call(t, h, "reminder-cancel", "ap", "during-check", map[string]any{"id": id, "version": 1})
		}
		return read(ctx, project, command, input)
	}
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 0 {
		t.Fatal("cancellation raced through external read")
	}
}

func TestCRMLinkedEditStagesApprovalWithoutChangingReminder(t *testing.T) {
	h, _, _, _ := fixture(t)
	plan := planFixture(h, "ag", 1)
	applyPlan(t, h, plan)
	currentBridge(h, &plan)
	var id string
	planStore(t, h, func(st *state) { id = st.Plans[plan.key()].ReminderID })
	want := h.now().Add(2 * time.Hour).Format(time.RFC3339)
	h.crmStage = func(_ context.Context, p CRMPlan, changes map[string]any, who core.ActionPrincipal, token string, _ core.Language) (map[string]any, *core.ActionHostResult, error) {
		if p != plan || changes["next_followup_at"] != want || who.Project != "ap" || token != "trusted-session" {
			t.Fatal("edit lost trusted binding")
		}
		return map[string]any{"status": "pending"}, &core.ActionHostResult{Kind: "crm.followup.v1", Status: "pending"}, nil
	}
	raw, _ := json.Marshal(map[string]any{"id": id, "version": 1, "at": want})
	result, card, err := h.Tool(context.Background(), "reminder-update", raw, principal("ap", "edit"), "trusted-session", core.LangChinese)
	if err != nil || result["status"] != "pending" || card == nil {
		t.Fatal("CRM approval not returned", result, err)
	}
	planStore(t, h, func(st *state) {
		if st.Items[id].Version != 1 || st.Items[id].At != h.now().Add(time.Hour).Unix() {
			t.Fatal("reminder changed before CRM approval")
		}
	})
	result, _, err = h.Tool(context.Background(), "reminder-update", raw, principal("bp", "foreign"), "foreign", core.LangChinese)
	if err != nil || result["code"] != "not_found" {
		t.Fatal("foreign user could edit CRM-linked notification")
	}
	read := h.crmCall
	h.crmCall = func(ctx context.Context, project, command string, input map[string]any) (map[string]any, error) {
		if command == "host-plan-read" && project == "ap" {
			return map[string]any{"status": "blocked", "code": "plan_namespace_disabled"}, nil
		}
		return read(ctx, project, command, input)
	}
	h.crmStage = func(context.Context, CRMPlan, map[string]any, core.ActionPrincipal, string, core.Language) (map[string]any, *core.ActionHostResult, error) {
		t.Fatal("linked update crossed CRM environments")
		return nil, nil, nil
	}
	result, card, err = h.Tool(context.Background(), "reminder-update", raw, principal("ap", "different-base"), "trusted-session", core.LangChinese)
	if err != nil || card != nil || result["code"] != "crm_plan_environment_changed" {
		t.Fatal("same customer number/revision in another Base accepted", result, err)
	}
}

func TestCRMStoreUpgradeKeepsOldCodeFromDiscardingLinks(t *testing.T) {
	h, _, _, _ := fixture(t)
	planStore(t, h, func(st *state) {
		if st.Schema != 1 {
			t.Fatal("ordinary store unnecessarily upgraded")
		}
	})
	plan := planFixture(h, "ap", 1)
	applyPlan(t, h, plan)
	planStore(t, h, func(st *state) {
		if st.Schema != 2 || !validState(*st) {
			t.Fatal("linked state was not versioned")
		}
		raw, _ := json.Marshal(st)
		// The old decoder discards new fields, but its required Schema==1 check
		// must still reject this header before any write or ordinary delivery.
		var old struct {
			Schema int                        `json:"schema"`
			Items  map[string]json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &old) != nil || old.Schema == 1 {
			t.Fatal("old binary could accept linked state")
		}
		copy := *st
		copy.Schema = 1
		if validState(copy) {
			t.Fatal("schema downgrade accepted CRM links")
		}
		copy = *st
		copy.Plans = nil
		if validState(copy) {
			t.Fatal("lost plan index accepted")
		}
		id := st.Plans[plan.key()].ReminderID
		link := st.Items[id].CRM
		st.Items[id].CRM = nil
		if validState(*st) {
			t.Fatal("lost item linkage accepted")
		}
		st.Items[id].CRM = link
	})
}

func TestCRMUnavailableHelperDoesNotBlockPersonalScheduler(t *testing.T) {
	h, sender, now, _ := fixture(t)
	create(t, h, "ap", "ordinary", "Deliver without CRM")
	*now = now.Add(time.Hour)
	entered, delivered, done := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	sender.hook = func() {
		select {
		case delivered <- struct{}{}:
		default:
		}
	}
	h.EnableCRM(func(ctx context.Context, _, _ string, _ map[string]any) (map[string]any, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { h.Run(ctx, core.LangChinese); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("CRM lane not started")
	}
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("CRM helper blocked ordinary reminder")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("bounded CRM lane did not stop")
	}
}

func TestCRMFeatureOffPausesPersistedPlanAndNewRevisionReplacesPause(t *testing.T) {
	h, sender, now, path := fixture(t)
	one := planFixture(h, "ap", 1)
	applyPlan(t, h, one)
	var oldID string
	planStore(t, h, func(st *state) { oldID = st.Plans[one.key()].ReminderID })
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h.store = store // Feature remains disabled: no EnableCRM callback.
	t.Cleanup(func() { _ = store.Close() })
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	planStore(t, h, func(st *state) {
		if st.Items[oldID].Status != "paused" || len(sender.calls) != 0 {
			t.Fatal("disabled CRM notification fell through as personal")
		}
	})
	two := planFixture(h, "bg", 2)
	applyPlan(t, h, two)
	currentBridge(h, &two)
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	planStore(t, h, func(st *state) {
		if st.Items[oldID].Status != "cancelled" || st.Items[st.Plans[two.key()].ReminderID].Status != "sent" || len(sender.calls) != 1 || sender.calls[0].chat != "pb" {
			t.Fatal("new explicit revision did not replace paused notification")
		}
	})
}

func TestCRMClaimLeaseIncludesReadBudgetWithoutChangingPersonalLease(t *testing.T) {
	h, _, now, _ := fixture(t)
	applyPlan(t, h, planFixture(h, "ap", 1))
	create(t, h, "ap", "ordinary", "Personal lease remains unchanged")
	*now = now.Add(time.Hour)
	planStore(t, h, func(st *state) {
		h.collectDue(st, core.LangChinese)
		crm, personal := h.claimBatchScope(st, core.LangChinese, 2), h.claimBatchScope(st, core.LangChinese, 1)
		if crm == nil || personal == nil || crm.LeaseUntil != now.Add(90*time.Second).Unix() || personal.LeaseUntil != now.Add(time.Minute).Unix() {
			t.Fatal("CRM verification consumed the ordinary delivery lease margin")
		}
	})
}
