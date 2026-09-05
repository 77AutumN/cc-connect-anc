package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type nextCardPlatform struct {
	hostedCardPlatform
	reconstructErr error
	reconstructed  string
	activationErr  error
}

func (p *nextCardPlatform) RefreshCardMessage(ctx context.Context, id, key string, card *Card) error {
	if p.activationErr != nil && card.Header.Title == "Separate follow-up approval" {
		return p.activationErr
	}
	return p.hostedCardPlatform.RefreshCardMessage(ctx, id, key, card)
}

func (p *nextCardPlatform) ReconstructReplyCtx(key string) (any, error) {
	p.reconstructed = key
	return key, p.reconstructErr
}

func TestHostedNextApprovalPublishesOnceAndKeepsParentReceipt(t *testing.T) {
	for _, variant := range []string{"success", "publish", "reconstruct", "bind", "activate", "nested", "non-pending", "wrong-kind"} {
		t.Run(variant, func(t *testing.T) {
			p := &nextCardPlatform{hostedCardPlatform: hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
			h := &actionHostStub{}
			next := &ActionHostResult{Kind: h.Kind(), Status: "pending", ApprovalID: "child", Card: NewCard().Title("Separate follow-up approval", "blue").Build()}
			parent := ActionHostResult{Kind: h.Kind(), Status: "verified", Card: NewCard().Title("Customer verified", "green").Build(), Next: next}
			h.executeResult = &parent
			switch variant {
			case "publish":
				p.publishErr = errors.New("fixture delivery failure")
			case "reconstruct":
				p.reconstructErr = errors.New("fixture context failure")
			case "bind":
				h.bindErr = errors.New("fixture binding failure")
			case "activate":
				p.activationErr = errors.New("fixture activation failure")
			case "nested":
				next.Next = &ActionHostResult{Status: "pending"}
			case "non-pending":
				next.Status = "cancelled"
			case "wrong-kind":
				next.Kind = "other.action"
			}
			e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
			e.SetActionHost(h)
			action := TrustedCardAction{Kind: h.Kind(), ApprovalID: "parent", Decision: ActionApprove,
				Principal: ActionPrincipal{UserID: "owner", ChatID: "chat", SessionKey: "test:chat:owner", MessageID: "parent-card"}}
			response := e.handleTrustedCardAction(p, action)
			if response.Complete == nil {
				t.Fatal("no claimed continuation")
			}
			first, repeat := response.Complete(), response.Complete()
			if len(h.executions) != 1 || first.Card != repeat.Card || first.Card.Header.Color != "green" {
				t.Fatal("completion replay executed twice or lost parent success")
			}
			if strings.Contains(parent.Card.RenderText(), "follow-up was not submitted") {
				t.Fatal("shared parent receipt was mutated")
			}
			if variant == "success" {
				if len(p.placeholders) != 1 || len(h.cardBindings) != 1 || len(p.refreshes) != 1 {
					t.Fatal("child not published exactly once")
				}
				bound := h.cardBindings[0]
				if bound.UserID != "owner" || bound.ChatID != "chat" || bound.Project != "project" || bound.Platform != "test" || bound.SessionKey != action.Principal.SessionKey || bound.MessageID != "outgoing-approval-card" || p.reconstructed != action.Principal.SessionKey {
					t.Fatalf("child binding changed principal or retained parent card: %+v", bound)
				}
			} else if !strings.Contains(first.Card.RenderText(), "follow-up was not submitted") {
				t.Fatalf("delivery failure was not distinguished from parent success: %s", first.Card.RenderText())
			}
			// Durable Claim is authoritative on a replay. Even a stale Next on
			// the claimed receipt must not cause another child publication.
			h.claimResult = &parent
			h.claimExecute = false
			replayed := e.handleTrustedCardAction(p, action)
			if replayed.Complete != nil || len(h.executions) != 1 {
				t.Fatal("callback replay re-executed parent")
			}
		})
	}
}
