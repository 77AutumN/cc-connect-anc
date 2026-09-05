package crmfollowup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestCustomerProfileCardsKeepTwoApprovalsAndHideInternalFields(t *testing.T) {
	for _, op := range []string{"customer_create", "customer_update"} {
		profile := map[string]any{"operation_type": op, "fields": map[string]any{"name": "虚构客户", "stage": "新线索", "owner": []any{map[string]any{"id": "ou_SECRET", "name": "示例同事"}}, "record_id": "rec_SECRET", "owner_ref": "ref_SECRET"}}
		if op == "customer_update" {
			profile["changes"] = []any{map[string]any{"field": "contact", "before": "旧联系人", "after": "新联系人", "actual": "新联系人"}, map[string]any{"field": "record_id", "before": "rec_SECRET", "after": "rec_NEW"}}
		}
		preview := map[string]any{"operation_type": op, "actor": "示例用户", "customer": map[string]any{"name": "虚构客户"}, "effects": map[string]any{"customer_profile": profile}, "followup_draft": map[string]any{"content": "需要演示", "occurred_at": "2026-09-06T10:00:00+08:00"}, "expires_at": "2026-09-06T10:10:00+08:00"}
		result := map[string]any{"status": "pending", "approval_id": "apr_SECRET", "preview": preview}
		before, _ := json.Marshal(result)
		visible := approvalCard(result, core.LangChinese).RenderText()
		for _, want := range []string{"客户资料", "尚未批准", "第二笔独立批准", "需要演示", "2026-09-06 10:10:00"} {
			if !strings.Contains(visible, want) {
				t.Errorf("%s missing %q: %s", op, want, visible)
			}
		}
		for _, hidden := range []string{"record_id", "owner_ref", "rec_SECRET", "ref_SECRET", "ou_SECRET", "新增并回读跟进"} {
			if strings.Contains(visible, hidden) {
				t.Errorf("unexpected %q in %s", hidden, visible)
			}
		}
		after, _ := json.Marshal(result)
		if string(before) != string(after) {
			t.Fatal("canonical preview mutated")
		}
		code := "customer_created_verified"
		if op == "customer_update" {
			code = "customer_updated_verified"
		}
		receipt := receiptCard(map[string]any{"status": "verified", "code": code, "customer_profile": profile, "customer": map[string]any{"name": "虚构客户"}, "child_change_id": "chg_child"}, core.LangChinese)
		if receipt.Header.Color != "green" || !strings.Contains(receipt.RenderText(), "回读核验") || !strings.Contains(receipt.RenderText(), "另行批准") {
			t.Fatal(receipt.RenderText())
		}
	}
	for _, status := range []string{"cancelled", "superseded", "expired", "replan_required"} {
		card := receiptCard(map[string]any{"status": status, "parent_operation": map[string]any{"status": "verified"}}, core.LangChinese)
		if strings.Contains(card.Header.Title, "飞书未写入") || !strings.Contains(card.RenderText(), "客户操作已完成") || !strings.Contains(card.RenderText(), "不会撤销") {
			t.Fatal(card.RenderText())
		}
	}
}

func TestCustomerToolRoutesReuseCanonicalApprovalAndNextOnlyOnExecute(t *testing.T) {
	for _, command := range []string{"assignee", "stage-customer-create", "stage-customer-update"} {
		a := &Adapter{toolsEnabled: true, run: func(_ context.Context, _, subcommand string, input []byte, _ []string) ([]byte, error) {
			var data map[string]any
			_ = json.Unmarshal(input, &data)
			if subcommand != "host-tool" || data["command"] != command {
				t.Fatal("tool route changed")
			}
			return []byte(`{"status":"pending","approval_id":"apr_fixture","change_id":"chg_fixture","preview":{"operation_type":"customer_create"}}`), nil
		}}
		_, card, err := a.Tool(context.Background(), command, json.RawMessage(`{}`), core.ActionPrincipal{}, "token", core.LangChinese)
		if err != nil || (card != nil) != (command != "assignee") {
			t.Fatalf("%s: %v %v", command, card, err)
		}
	}
	for _, tc := range []struct {
		status, next string
		want         bool
	}{{"verified", `{"status":"pending","approval_id":"child","change_id":"chg_child","preview":{}}`, true}, {"partial", `{"status":"pending"}`, false}, {"verified", `{"status":"verified"}`, false}, {"verified", `{"status":"pending","next_approval":{}}`, false}} {
		a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
			return []byte(`{"status":"` + tc.status + `","next_approval":` + tc.next + `}`), nil
		}}
		result, err := a.Execute(context.Background(), "parent", core.ActionPrincipal{}, core.LangEnglish)
		if err != nil || (result.Next != nil) != tc.want {
			t.Fatalf("Execute next: %+v %v", result, err)
		}
		claim, execute, err := a.Claim(context.Background(), "parent", core.ActionApprove, core.ActionPrincipal{}, core.LangEnglish)
		if err != nil || execute || claim.Next != nil {
			t.Fatal("Claim replay exposed next approval")
		}
	}
}

func TestUnpublishedFollowupIsExplicitInActualHostExecuteReceipt(t *testing.T) {
	for _, state := range []string{"unavailable", "not_published"} {
		a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
			return []byte(`{"status":"verified","code":"customer_created_verified","next_approval_status":"` + state + `"}`), nil
		}}
		result, err := a.Execute(context.Background(), "parent", core.ActionPrincipal{}, core.LangChinese)
		if err != nil || result.Next != nil || result.Card == nil || result.Card.Header.Color != "green" {
			t.Fatalf("parent success was lost: %+v %v", result, err)
		}
		for _, want := range []string{"客户资料已创建", "未生成或发布", "未提交跟进", "人工恢复", "不要重复客户操作"} {
			if !strings.Contains(result.Card.RenderText(), want) {
				t.Fatalf("%s missing %q: %s", state, want, result.Card.RenderText())
			}
		}
	}
}

func TestClosedChildRepairDoesNotDenyExistingFollowup(t *testing.T) {
	for _, state := range []string{"cancelled", "superseded", "expired"} {
		card := receiptCard(map[string]any{"status": state, "code": "repair_closed_partial",
			"durable_change_state": "partial", "parent_operation": map[string]any{"status": "verified"},
			"followup": map[string]any{"content": "已写入的虚构跟进"}}, core.LangChinese)
		visible := card.RenderText()
		if card.Header.Color != "orange" || !strings.Contains(visible, "已写入跟进保留") || !strings.Contains(visible, "已写入的虚构跟进") {
			t.Fatal(visible)
		}
		for _, forbidden := range []string{"跟进未提交", "飞书未写入", "取消操作未改变"} {
			if strings.Contains(visible, forbidden) {
				t.Fatalf("false no-write claim %q: %s", forbidden, visible)
			}
		}
	}
}
