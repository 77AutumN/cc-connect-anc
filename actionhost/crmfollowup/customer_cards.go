package crmfollowup

import (
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

var customerProfileFields = []string{"name", "contact", "phone", "email", "stage", "owner", "next_action", "next_followup_at"}

// Only business fields from the canonical helper result are displayable. Tokens,
// record IDs and transport identity never become card labels or values.
func renderCustomerProfile(profile map[string]any, receipt bool, i18n *core.I18n) string {
	heading := core.MsgCRMProfileFieldsHeading
	if changes, ok := profile["changes"].([]any); ok && len(changes) > 0 {
		var lines []string
		for _, raw := range changes {
			change, _ := raw.(map[string]any)
			field := text(change["field"])
			if fieldLabel(field, i18n) == "" {
				continue
			}
			value := change["after"]
			if receipt {
				value = change["actual"]
				if _, present := change["actual"]; !present {
					value = i18n.T(core.MsgCRMNotReadBack)
				}
			}
			lines = append(lines, fmt.Sprintf("- **%s**: %s → %s", fieldLabel(field, i18n), displayField(field, change["before"], i18n), displayField(field, value, i18n)))
		}
		return i18n.T(core.MsgCRMPreviewChangesHeading) + "\n" + strings.Join(lines, "\n")
	}
	fields, _ := profile["fields"].(map[string]any)
	return i18n.T(heading) + "\n" + renderFields(fields, customerProfileFields, i18n)
}

func renderCustomerPreview(preview map[string]any, i18n *core.I18n) string {
	var sections []string
	if actor := text(preview["actor"]); actor != "" {
		sections = append(sections, i18n.Tf(core.MsgCRMPreviewActorFmt, display(actor)))
	}
	if customer, ok := preview["customer"].(map[string]any); ok {
		sections = append(sections, i18n.T(core.MsgCRMPreviewTargetHeading)+"\n"+renderFields(customer, append([]string{"customer_number"}, customerProfileFields...), i18n))
		if link := safeLink(customer["url"]); link != "" {
			sections = append(sections, "["+i18n.T(core.MsgCRMOpenCustomer)+"]("+link+")")
		}
	}
	effects, _ := preview["effects"].(map[string]any)
	if profile, ok := effects["customer_profile"].(map[string]any); ok {
		sections = append(sections, renderCustomerProfile(profile, false, i18n))
	}
	if candidates, ok := preview["same_name_customers"].([]any); ok && len(candidates) > 0 {
		sections = append(sections, i18n.T(core.MsgCRMSameNameHeading)+"\n"+renderCandidates(candidates, i18n))
	}
	if draft, ok := preview["followup_draft"].(map[string]any); ok {
		sections = append(sections, i18n.T(core.MsgCRMFollowupDraftHeading)+"\n"+renderFields(draft, []string{"occurred_at", "channel", "content", "next_action", "next_followup_at"}, i18n))
	}
	sections = append(sections, i18n.Tf(core.MsgCRMProfileExpiryBlockFmt, displayField("expires_at", preview["expires_at"], i18n)))
	if plan := renderPlanReminder(preview, i18n); plan != "" {
		sections = append(sections, plan)
	}
	return strings.Join(sections, "\n\n")
}

func renderPlanReminder(preview map[string]any, i18n *core.I18n) string {
	plan, ok := preview["plan_reminder"].(map[string]any)
	if !ok {
		return ""
	}
	if enabled, _ := plan["enabled"].(bool); !enabled {
		return i18n.T(core.MsgCRMPlanSilent)
	}
	return i18n.Tf(core.MsgCRMPlanNotify, displayField("next_followup_at", plan["at"], i18n), display(plan["recipient"]))
}

func relatedParent(result map[string]any) map[string]any {
	if parent, ok := result["parent_operation"].(map[string]any); ok {
		return parent
	}
	related, _ := result["related_operations"].(map[string]any)
	parent, _ := related["parent"].(map[string]any)
	return parent
}
