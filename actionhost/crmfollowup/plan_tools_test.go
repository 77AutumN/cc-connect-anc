package crmfollowup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestCRMPlanToolsPreserveHostBindingAndExpectedRevision(t *testing.T) {
	p := core.ActionPrincipal{Platform: "feishu", UserID: "fixture-user", ChatID: "fixture-chat", Project: "fixture-project", SessionKey: "session", MessageID: "message"}
	binding := map[string]any{"user_id": p.UserID, "source_chat_id": p.ChatID, "project": p.Project, "private_chat_id": "fixture-private"}
	var payload map[string]any
	a := &Adapter{toolsEnabled: true, run: func(_ context.Context, _, command string, raw []byte, _ []string) ([]byte, error) {
		if command != "host-tool" {
			t.Fatal(command)
		}
		payload = nil
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		return []byte(`{"status":"pending","approval_id":"apr_fixture","change_id":"chg_fixture","preview":{}}`), nil
	}}
	a.ConfigurePlanBinding(func(who core.ActionPrincipal) map[string]any {
		if who != p {
			t.Fatal("principal changed")
		}
		return binding
	})
	_, card, err := a.StagePlanUpdate(context.Background(), "C-001", "fixture-namespace", "fixture-record", 7, map[string]any{"next_followup_at": "2026-09-20T09:00:00+08:00"}, p, "trusted-token", core.LangChinese)
	if err != nil || card == nil || card.Kind != Kind || payload["expected_plan_revision"] != float64(7) || payload["command"] != "stage-customer-update" {
		t.Fatal("linked edit lost CRM approval", payload, err)
	}
	request := payload["request"].(map[string]any)
	if request["remind"] != true || request["customer_query"] != "C-001" || payload["plan_reminder_binding"].(map[string]any)["private_chat_id"] != "fixture-private" {
		t.Fatal("lost trusted plan binding")
	}
	target := payload["expected_plan_target"].(map[string]any)
	if target["namespace"] != "fixture-namespace" || target["customer_id"] != "fixture-record" || request["expected_plan_target"] != nil {
		t.Fatal("host-only target is missing or model-controlled")
	}
	for command, input := range map[string]string{"customers": `{"owner":"me","offset":20}`, "customer": `{"customer_query":"C-001","history_offset":10}`} {
		_, _, err = a.Tool(context.Background(), command, json.RawMessage(input), p, "trusted-token", core.LangChinese)
		if err != nil || payload["command"] != command || payload["plan_reminder_binding"] != nil || payload["expected_plan_revision"] != nil || payload["expected_plan_target"] != nil {
			t.Fatal("read request received a write binding", payload, err)
		}
	}
	a.ConfigurePlanBinding(func(core.ActionPrincipal) map[string]any { return nil })
	_, _, err = a.Tool(context.Background(), "stage", json.RawMessage(`{"content":"ordinary CRM follow-up"}`), p, "trusted-token", core.LangChinese)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["plan_reminder_binding"]; exists {
		t.Fatal("nil route binding broke an unrelated CRM stage")
	}
	result, card, err := a.StagePlanUpdate(context.Background(), "C-001", "fixture-namespace", "fixture-record", 7, map[string]any{}, p, "trusted-token", core.LangChinese)
	if err != nil || card != nil || result["code"] != "crm_plan_route_unavailable" {
		t.Fatal("linked edit accepted unknown private route")
	}
}

func TestCRMPlanApprovalExplainsReplacementWithoutPrivateIdentifiers(t *testing.T) {
	for _, operation := range []string{"followup", "customer_update"} {
		preview := map[string]any{"operation_type": operation, "actor": "Example operator", "plan_reminder": map[string]any{"enabled": true, "at": "2026-09-20T09:00:00+08:00", "recipient": "Example operator", "private_chat_id": "SECRET-CHAT", "namespace": "SECRET-BASE"}}
		card := approvalCard(map[string]any{"status": "pending", "approval_id": "apr_fixture", "preview": preview}, core.LangChinese).RenderText()
		if !strings.Contains(card, "其他同事") || !strings.Contains(card, "普通个人提醒不受影响") || !strings.Contains(card, "2026-09-20 09:00") || !strings.Contains(card, "Example operator") || strings.Contains(card, "SECRET") {
			t.Fatal("incomplete or leaking plan preview", card)
		}
		preview["plan_reminder"].(map[string]any)["enabled"] = false
		card = approvalCard(map[string]any{"status": "pending", "approval_id": "apr_fixture", "preview": preview}, core.LangChinese).RenderText()
		if !strings.Contains(card, "本计划不发通知") || strings.Contains(card, "SECRET") {
			t.Fatal("silent plan semantics missing", card)
		}
	}
	receipt := receiptCard(map[string]any{"status": "verified", "plan_reminder_status": "pending_sync"}, core.LangChinese).RenderText()
	if !strings.Contains(receipt, "通知状态以提醒清单为准") || !strings.Contains(receipt, "不表示已送达") {
		t.Fatal("CRM success overstated reminder state", receipt)
	}
}
