package teambrain

import (
	"github.com/chenhg5/cc-connect/core"
	"strings"
)

func stringValue(value any) string { result, _ := value.(string); return result }

func present(data map[string]any, lang core.Language) core.ActionHostResult {
	i18n := core.NewI18n(lang)
	status := stringValue(data["status"])
	result := core.ActionHostResult{Kind: Kind, Status: status, Code: stringValue(data["code"]),
		ApprovalID: stringValue(data["approval_id"]), ChangeID: stringValue(data["change_id"])}
	result.Replayed, _ = data["replayed"].(bool)
	title, color := core.MsgKnowledgeNeedsReview, "orange"
	switch status {
	case "pending":
		title, color = core.MsgKnowledgePreview, "blue"
	case "claimed", "executing":
		result.Status = "executing"
		title = core.MsgHostedActionProcessingToast
	case "verified":
		title, color = core.MsgKnowledgePublished, "green"
	case "unknown":
		result.Status, result.Code = "executing", "execution_result_unknown"
		title = core.MsgHostedActionUnknownTitle
	}
	card := core.NewCard().Title(i18n.T(title), color)
	if status == "pending" {
		// Fence data and neutralize fence delimiters. Never interpret source text as UI.
		preview := strings.ReplaceAll(stringValue(data["preview"]), "`", "ˋ")
		card.Markdown(i18n.T(core.MsgKnowledgeReviewBody)).Markdown("```\n" + preview + "\n```")
		button := func(key core.MsgKey, typ string, decision core.ActionDecision) core.CardButton {
			return core.CardButton{Text: i18n.T(key), Type: typ, Extra: map[string]string{
				"kind": Kind, "approval_id": result.ApprovalID, "decision": string(decision), "language": string(lang)}}
		}
		card.ButtonsEqual(button(core.MsgCRMApproveButton, "primary", core.ActionApprove),
			button(core.MsgCRMModifyButton, "default", core.ActionModify), button(core.MsgCRMCancelButton, "default", core.ActionCancel))
	} else if status == "verified" {
		if receipt, ok := data["receipt"].(map[string]any); ok {
			card.Markdown(stringValue(receipt["url"]))
			card.Markdown("```\n" + strings.ReplaceAll(stringValue(receipt["summary"]), "`", "ˋ") + "\n```")
		}
	} else if status != "claimed" && status != "executing" {
		card.Markdown(i18n.T(core.MsgKnowledgeReviewAgainBody))
	}
	result.Card = card.Build()
	return result
}
