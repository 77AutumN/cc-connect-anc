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
	if !a.toolsEnabled || token == "" || (!stages && command != "open" && command != "customer" && command != "result" && command != "assignee") {
		return map[string]any{"status": "blocked", "code": "unsupported_tool_or_session"}, nil, nil
	}
	payload := struct {
		Command   string               `json:"command"`
		Request   json.RawMessage      `json:"request"`
		Principal core.ActionPrincipal `json:"principal"`
	}{command, input, principal}
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
