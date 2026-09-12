package teambrain

import (
	"github.com/chenhg5/cc-connect/core"
	"net/url"
	"strings"
)

func stringValue(value any) string { result, _ := value.(string); return result }

func present(data map[string]any, lang core.Language) core.ActionHostResult {
	i18n := core.NewI18n(lang)
	status := stringValue(data["status"])
	result := core.ActionHostResult{Kind: Kind, Status: status, Code: stringValue(data["code"]),
		ApprovalID: stringValue(data["approval_id"]), ChangeID: stringValue(data["change_id"])}
	result.Replayed, _ = data["replayed"].(bool)
	title, color := core.MsgHostedActionUnknownTitle, "orange"
	switch status {
	case "pending":
		title, color = core.MsgKnowledgePreview, "blue"
	case "claimed", "executing":
		result.Status = "executing"
		title = core.MsgHostedActionProcessingToast
	case "verified":
		title, color = core.MsgKnowledgePublished, "green"
	case "cancelled":
		title = core.MsgKnowledgeCancelled
	case "needs_revision", "expired", "conflict":
		title = core.MsgKnowledgeNeedsReview
	case "unknown":
		result.Status, result.Code = "executing", "execution_result_unknown"
		title = core.MsgHostedActionUnknownTitle
	}
	card := core.NewCard().Title(i18n.T(title), color)
	if status == "pending" {
		card.PlainText(i18n.T(core.MsgKnowledgeReviewBody))
		reviewCard(card, data, i18n)
		button := func(key core.MsgKey, typ string, decision core.ActionDecision) core.CardButton {
			return core.CardButton{Text: i18n.T(key), Type: typ, Extra: map[string]string{
				"kind": Kind, "approval_id": result.ApprovalID, "decision": string(decision), "language": string(lang)}}
		}
		card.ButtonsEqual(button(core.MsgCRMApproveButton, "primary", core.ActionApprove),
			button(core.MsgCRMModifyButton, "default", core.ActionModify), button(core.MsgCRMCancelButton, "default", core.ActionCancel))
	} else if status == "verified" {
		if receipt, ok := data["receipt"].(map[string]any); ok {
			hostLink(card, stringValue(receipt["url"]))
		}
		card.PlainText(i18n.Tf(core.MsgKnowledgeDone, stringValue(data["title"])))
	} else if status != "claimed" && status != "executing" {
		key := core.MsgKnowledgeUncertain
		switch status {
		case "cancelled":
			key = core.MsgKnowledgeCancelled
		case "needs_revision":
			key = core.MsgKnowledgeRevision
		case "expired":
			key = core.MsgKnowledgeExpired
		case "conflict":
			key = core.MsgKnowledgeConflict
		}
		card.PlainText(i18n.T(key))
	}
	result.Card = card.Build()
	if status == "pending" {
		result.Card.MaxBytes = 24000
	}
	return result
}

func hostLink(card *core.CardBuilder, value string) {
	u, err := url.Parse(value)
	if err == nil && u.Scheme == "https" && u.Host != "" && !strings.ContainsAny(value, "<>[]()`\r\n\t ") {
		card.Markdown(value)
	} else {
		card.PlainText(value)
	}
}

func reviewCard(card *core.CardBuilder, data map[string]any, i18n *core.I18n) {
	review, ok := data["review"].(map[string]any)
	operation := stringValue(review["operation"])
	key, valid := map[string]core.MsgKey{"create": core.MsgKnowledgeCreate, "append": core.MsgKnowledgeAppend, "replace": core.MsgKnowledgeReplace}[operation]
	if !ok || !valid || stringValue(review["after"]) == "" {
		// Legacy approvals retain their entire frozen preview, including original punctuation.
		card.PlainText(stringValue(data["preview"]))
		return
	}
	card.PlainText(i18n.Tf(core.MsgKnowledgeReviewMeta, i18n.T(key), stringValue(review["title"])))
	card.PlainText(i18n.T(core.MsgKnowledgeDestination))
	hostLink(card, stringValue(review["destination"]))
	if operation != "create" {
		card.PlainText(i18n.T(core.MsgKnowledgeBefore)).PlainText(stringValue(review["before"]))
	}
	after := core.MsgKnowledgeAfter
	if operation == "append" {
		after = core.MsgKnowledgeAdded
	}
	card.PlainText(i18n.T(after)).PlainText(stringValue(review["after"]))
}
