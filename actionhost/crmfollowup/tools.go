package crmfollowup

import (
	"context"
	"encoding/json"

	"github.com/chenhg5/cc-connect/core"
)

// Tool is the opt-in, model-readable counterpart to the hosted card flow.
// The Engine supplies identity; the helper validates business fields and owns
// the ledger. A stage returns the same canonical facts to model and renderer.
// No filesystem marker, permission response, or agent stop is involved.
func (a *Adapter) Tool(ctx context.Context, command string, input json.RawMessage, principal core.ActionPrincipal, token string, lang core.Language) (map[string]any, *core.ActionHostResult, error) {
	stages := command == "stage" || command == "stage-customer-create" || command == "stage-customer-update"
	caseTool := command == "case-list" || command == "case-read" || command == "case-update"
	if !a.toolsEnabled || token == "" || (!caseTool && !stages && command != "open" && command != "customer" && command != "customers" && command != "result" && command != "assignee") {
		return map[string]any{"status": "blocked", "code": "unsupported_tool_or_session"}, nil, nil
	}
	payload := map[string]any{"command": command, "request": input, "principal": principal}
	if caseTool {
		binding, ok := core.TrustedCaseContext(ctx)
		if !ok {
			return map[string]any{"status": "blocked", "code": "case_source_unavailable"}, nil, nil
		}
		payload["case_context"] = binding
	}
	if stages && a.planBinding != nil {
		if binding := a.planBinding(principal); binding != nil {
			payload["plan_reminder_binding"] = binding
		}
	}
	data, err := a.call(ctx, "host-tool", payload, token)
	if err != nil {
		return nil, nil, err
	}
	if !stages || data["status"] != "pending" {
		return data, nil, nil
	}
	card := hostResult(data)
	card.Kind = Kind
	card.Card = approvalCard(data, lang)
	return data, &card, nil
}

// ConfigurePlanBinding runs only at host startup. No model field selects a route.
func (a *Adapter) ConfigurePlanBinding(binding func(core.ActionPrincipal) map[string]any) {
	a.planBinding = binding
}

// PlanCall is a host-only bridge on the existing subprocess boundary.
func (a *Adapter) PlanCall(ctx context.Context, command string, input map[string]any) (map[string]any, error) {
	if command != "host-plan-events" && command != "host-plan-read" && command != "host-plan-ack" {
		return map[string]any{"status": "blocked"}, nil
	}
	return a.call(ctx, command, input, "")
}

// StagePlanUpdate preserves the normal CRM card and sender approval, pinning the
// linked version so an old reminder cannot edit a newer shared plan.
func (a *Adapter) StagePlanUpdate(ctx context.Context, query, namespace, customerID string, revision int, changes map[string]any, principal core.ActionPrincipal, token string, lang core.Language) (map[string]any, *core.ActionHostResult, error) {
	if !a.toolsEnabled || token == "" || a.planBinding == nil {
		return map[string]any{"status": "blocked", "code": "crm_plan_disabled"}, nil, nil
	}
	if namespace == "" || customerID == "" || revision < 1 {
		return map[string]any{"status": "blocked", "code": "crm_plan_target_unavailable"}, nil, nil
	}
	binding := a.planBinding(principal)
	if binding == nil {
		return map[string]any{"status": "blocked", "code": "crm_plan_route_unavailable"}, nil, nil
	}
	payload := map[string]any{"command": "stage-customer-update", "request": map[string]any{"customer_query": query, "changes": changes, "remind": true}, "principal": principal, "expected_plan_revision": revision, "expected_plan_target": map[string]any{"namespace": namespace, "customer_id": customerID}, "plan_reminder_binding": binding}
	data, err := a.call(ctx, "host-tool", payload, token)
	if err != nil || data["status"] != "pending" {
		return data, nil, err
	}
	card := hostResult(data)
	card.Kind, card.Card = Kind, approvalCard(data, lang)
	return data, &card, nil
}
