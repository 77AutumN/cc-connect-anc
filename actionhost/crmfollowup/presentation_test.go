package crmfollowup

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestCRMCardPresentationFormatsKnownFieldsWithoutChangingApproval(t *testing.T) {
	result := map[string]any{
		"status": "pending", "approval_id": "apr_example",
		"preview": map[string]any{
			"expires_at": "2026-09-05T09:16:28.850150Z",
			"customer": map[string]any{
				"name":  "<CRM_DATA>Customer [*x*] <at id=all></CRM_DATA>",
				"owner": []any{}, "last_followup_at": "<CRM_DATA>2026-09-03T11:00:00.000Z</CRM_DATA>",
				"next_followup_at": "2026-09-04T10:00:00.000+08:00",
				"url":              "https://example.feishu.cn/base/test",
			},
			"effects": map[string]any{
				"followup": map[string]any{"after": map[string]any{
					"occurred_at": "2026-09-05T08:55:00-00:00",
					"content":     "literal 2026-09-05T08:55:00Z must not be reformatted",
				}},
				"customer": map[string]any{"changes": []any{
					map[string]any{"field": "last_followup_at", "before": "2026-09-03T19:00:00+08:00", "after": "2026-09-05T16:55:00+08:00"},
				}},
			},
		},
	}
	before, _ := json.Marshal(result)
	card := approvalCard(result, core.LangChinese)
	visible := card.RenderText()
	for _, want := range []string{
		"2026-09-03 19:00（北京时间）", "2026-09-05 16:55（北京时间）",
		"2026-09-04 10:00（北京时间）", "2026-09-05 17:16:28（北京时间）",
		"负责人**: 未设置", "2026-09-03 19:00（北京时间） → 2026-09-05 16:55（北京时间）",
		"literal 2026-09-05T08:55:00Z must not be reformatted",
		`Customer \[\*x\*\] &lt;at id=all&gt;`, "https://example.feishu.cn/base/test",
	} {
		if !strings.Contains(visible, want) {
			t.Errorf("missing %q in %s", want, visible)
		}
	}
	for _, forbidden := range []string{"09:16:28.850150Z", "<CRM_DATA>", "<at id=all>"} {
		if strings.Contains(visible, forbidden) {
			t.Errorf("unsafe/unformatted value %q in %s", forbidden, visible)
		}
	}
	after, _ := json.Marshal(result)
	if string(before) != string(after) {
		t.Fatal("rendering mutated canonical preview or expiry")
	}
}

func TestCRMCardPresentationOwnersUseNamesNotArraysOrIDs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"null", nil, "未设置"}, {"empty array", []any{}, "未设置"}, {"empty text", "", "未设置"},
		{"text name", "<CRM_DATA>王小明</CRM_DATA>", "王小明"},
		{"person", map[string]any{"name": "<CRM_DATA>李 [总]</CRM_DATA>", "id": "ou_private"}, `李 \[总\]`},
		{"people", []any{map[string]any{"name": "王小明", "id": "ou_private"}, map[string]any{"name": "李小红", "id": "ou_private2"}}, "王小明、李小红"},
		{"unknown name", []any{map[string]any{"id": "ou_private"}}, "姓名不可用"},
		{"raw id", "ou_private", "姓名不可用"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderFields(map[string]any{"owner": tc.value}, []string{"owner"}, core.NewI18n(core.LangChinese))
			if !strings.Contains(got, tc.want) || strings.Contains(got, "ou_private") || strings.Contains(got, "map[") || strings.Contains(got, "[]") {
				t.Fatalf("owner render = %s; want %s", got, tc.want)
			}
		})
	}
}

func TestCRMCardPresentationDatesRespectOffsetsAcrossAllLanguages(t *testing.T) {
	for _, tc := range []struct {
		lang   core.Language
		suffix string
		unset  string
	}{
		{core.LangEnglish, " (Beijing time)", "Not set"},
		{core.LangChinese, "（北京时间）", "未设置"},
		{core.LangTraditionalChinese, "（北京時間）", "未設定"},
		{core.LangJapanese, "（北京時間）", "未設定"},
		{core.LangSpanish, " (hora de Pekín)", "Sin asignar"},
	} {
		i18n := core.NewI18n(tc.lang)
		for _, field := range []string{"occurred_at", "last_followup_at", "next_followup_at", "created_at", "updated_at"} {
			if got := displayField(field, "2026-09-05T20:30:45.123-04:00", i18n); got != "2026-09-06 08:30"+tc.suffix {
				t.Errorf("%s/%s = %s", tc.lang, field, got)
			}
		}
		if got := displayField("expires_at", "2026-09-05T20:30:45.123-04:00", i18n); got != "2026-09-06 08:30:45"+tc.suffix {
			t.Errorf("%s expiry = %s", tc.lang, got)
		}
		if got := displayField("owner", []any{}, i18n); got != tc.unset {
			t.Errorf("%s empty owner = %s", tc.lang, got)
		}
	}
}

func TestCRMReceiptPresentationLocalizesOutcomesWithoutHelperMessages(t *testing.T) {
	for _, tc := range []struct{ status, code, want string }{
		{"verified", "followup_and_customer_verified", "跟进记录与客户摘要均已回读核验"},
		{"verified", "followup_recorded_customer_unchanged", "保留了较新的客户摘要"},
		{"verified", "already_applied", "未重复写入"},
		{"verified", "partial_already_completed", "未重复写入"},
		{"partial", "customer_update_failed", "保留"},
		{"partial", "customer_changed_after_followup", "发生变化"},
		{"partial", "partial_recheck_failed", "人工核对"},
		{"verification_unknown", "followup_write_unconfirmed", "跟进写入结果尚未确认"},
		{"verification_unknown", "customer_write_unconfirmed", "客户摘要写入结果尚未确认"},
		{"verification_unknown", "idempotency_conflict", "人工核对"},
		{"verification_unknown", "followup_presence_unknown", "人工核对"},
		{"verification_unknown", "partial_followup_unverified", "人工核对"},
		{"verification_unknown", "apply_in_progress", "人工核对"},
		{"executing", "apply_in_progress", "执行"},
		{"executing", "execution_result_unknown", "人工核对"},
		{"cancelled", "", "未执行此方案"},
		{"superseded", "", "旧审批已失效"},
		{"expired", "", "已过期"},
		{"replan_required", "customer_changed", "发生变化"},
		{"replan_required", "schema_changed", "发生变化"},
		{"failed", "prewrite_read_failed", "写入前检查未通过"},
		{"failed", "ledger_unavailable", "人工核对"},
		{"blocked", "principal_mismatch", "未执行此请求"},
		{"new_unknown_state", "new_unknown_code", "人工核对"},
	} {
		t.Run(tc.status+"/"+tc.code, func(t *testing.T) {
			for _, lang := range []core.Language{core.LangEnglish, core.LangChinese, core.LangTraditionalChinese, core.LangJapanese, core.LangSpanish} {
				card := receiptCard(map[string]any{"status": tc.status, "code": tc.code, "message": "RAW_HELPER /etc/private token=not-real", "change_id": "chg_example"}, lang)
				got := card.RenderText()
				if strings.Contains(got, "RAW_HELPER") || strings.Contains(got, "/etc/private") || strings.Contains(got, "token=") {
					t.Fatalf("%s leaked helper message: %s", lang, got)
				}
				if lang == core.LangChinese && !strings.Contains(got, tc.want) {
					t.Errorf("missing %q in %s", tc.want, got)
				}
				if !strings.Contains(got, "chg_example") {
					t.Fatal("receipt lost operation reference")
				}
				for _, element := range card.Elements {
					if _, ok := element.(core.CardActions); ok {
						t.Fatal("receipt must not keep live approval buttons")
					}
				}
			}
		})
	}
}

func TestCRMCardPresentationLinksCannotCloseMarkdownTargets(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://example.feishu.cn/base/test_(copy)", "https://example.feishu.cn/base/test_%28copy%29"},
		{"<CRM_DATA>https://example.feishu.cn/base/test</CRM_DATA>", "https://example.feishu.cn/base/test"},
		{"javascript:alert(1)", ""},
		{"https://example.feishu.cn/\n@all", ""},
	} {
		if got := safeLink(tc.raw); got != tc.want {
			t.Errorf("link = %q, want %q", got, tc.want)
		}
	}
}

func TestCRMPartialReceiptRequiresManualReviewNotAnotherApproval(t *testing.T) {
	for _, tc := range []struct {
		lang      core.Language
		manual    string
		forbidden string
	}{
		{core.LangEnglish, "an administrator for manual review using the operation ID", "approved repair"},
		{core.LangChinese, "提供操作 ID，联系管理员人工核对", "批准修复"},
		{core.LangTraditionalChinese, "提供操作 ID，聯絡管理員人工核對", "批准修復"},
		{core.LangJapanese, "操作 ID を添えて管理者に手動確認", "承認してください"},
		{core.LangSpanish, "un administrador para una revisión manual con el ID de operación", "reparación"},
	} {
		for _, code := range []string{"customer_update_failed", "customer_changed_after_followup", "schema_changed_after_partial", "partial_recheck_failed"} {
			got := receiptMessage("partial", code, core.NewI18n(tc.lang))
			if !strings.Contains(got, tc.manual) || strings.Contains(got, tc.forbidden) {
				t.Errorf("%s/%s must lead to manual review, got: %s", tc.lang, code, got)
			}
		}
	}
}

func TestCRMReceiptPresentationReadbackDatesAndMissingActual(t *testing.T) {
	result := map[string]any{
		"status": "verification_unknown", "code": "customer_write_unconfirmed",
		"followup": map[string]any{"occurred_at": "2026-09-05T08:55:00.000Z"},
		"customer_changes": []any{
			map[string]any{"field": "last_followup_at", "before": "2026-09-03T19:00:00+08:00", "actual": "2026-09-05T08:55:00Z", "verified": true},
			map[string]any{"field": "next_action", "before": "prepare", "after": "demo"},
		},
	}
	got := receiptCard(result, core.LangChinese).RenderText()
	for _, want := range []string{"2026-09-05 16:55（北京时间）", "2026-09-03 19:00（北京时间） → 2026-09-05 16:55（北京时间）", "prepare → 未回读"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	invalid := renderFields(map[string]any{"occurred_at": "<CRM_DATA>invalid [time]</CRM_DATA>"}, []string{"occurred_at"}, core.NewI18n(core.LangChinese))
	if !strings.Contains(invalid, `invalid \[time\]`) || strings.Contains(invalid, "北京时间") {
		t.Fatalf("invented invalid date: %s", invalid)
	}
}
