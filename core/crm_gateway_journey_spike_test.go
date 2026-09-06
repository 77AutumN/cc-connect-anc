package core

// OFFLINE DESIGN SPIKE, not a production CRM transport or a model eval.
// Real: Engine, SessionManager, action-card publication/binding and callback
// routing. Fake: Agent process, Platform and one fixed CRM fixture. The direct
// tool closure below stands in for authenticated local IPC THAT DOES NOT EXIST
// YET. Python ledger safety/persistence is exercised separately, not copied here.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type gatewaySpikeAgent struct {
	cujAgent
	tool func(string) string
}

func (a *gatewaySpikeAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	as, err := a.cujAgent.StartSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return &gatewaySpikeSession{cujAgentSession: as.(*cujAgentSession), tool: a.tool}, nil
}

type gatewaySpikeSession struct {
	*cujAgentSession
	tool        func(string) string
	permissions atomic.Int32
}

func (s *gatewaySpikeSession) Send(prompt, messageID string, images []ImageAttachment, files []FileAttachment) error {
	// Deterministic scripted agent: wording/intent understanding is NOT graded.
	result := s.tool(prompt)
	s.mu.Lock()
	s.pendingEvents = []Event{
		{Type: EventThinking, Content: "SPIKE_INTERNAL_THINKING"},
		{Type: EventToolUse, ToolName: "SPIKE_INTERNAL_READ_OR_STAGE"},
		{Type: EventResult, Content: result, Done: true},
	}
	s.mu.Unlock()
	return s.cujAgentSession.Send(prompt, messageID, images, files)
}

func (s *gatewaySpikeSession) RespondPermission(string, PermissionResult) error {
	s.permissions.Add(1)
	return fmt.Errorf("this spike must not request permission interception")
}

type gatewaySpikePlatform struct {
	hostedCardPlatform
	callback TrustedCardActionHandler
}

func (p *gatewaySpikePlatform) SetTrustedCardActionHandler(h TrustedCardActionHandler) {
	p.callback = h
}

func (p *gatewaySpikePlatform) RefreshCardMessage(ctx context.Context, messageID, sessionKey string, card *Card) error {
	if err := p.hostedCardPlatform.RefreshCardMessage(ctx, messageID, sessionKey, card); err != nil {
		return err
	}
	// Record what the fake platform user sees after the original card refresh.
	return p.Reply(ctx, messageID, card.RenderText())
}

// One fixed host fixture, NOT a second ledger/state-machine implementation.
// Its binding/duplicate response lets us exercise the real Engine callback
// branches; it is not evidence for Python CAS, TTL, drift or crash recovery.
type gatewaySpikeHost struct {
	actionHostStub
	approved bool
}

func (h *gatewaySpikeHost) Claim(_ context.Context, id string, decision ActionDecision, p ActionPrincipal, _ Language) (ActionHostResult, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.cardBindings) != 1 || p != h.cardBindings[0] || id != h.beginResult.ApprovalID {
		return ActionHostResult{Code: "principal_mismatch"}, false, nil
	}
	if h.approved {
		return h.receipt(), false, nil
	}
	if decision != ActionApprove {
		return ActionHostResult{Status: "cancelled", Card: NewCard().Markdown("Cancelled; no CRM write.").Build()}, false, nil
	}
	h.approved = true
	return ActionHostResult{Status: "executing", Card: NewCard().Title("Executing original plan", "blue").Build()}, true, nil
}

func (h *gatewaySpikeHost) Execute(_ context.Context, id string, p ActionPrincipal, _ Language) (ActionHostResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.executions = append(h.executions, id)
	h.principals = append(h.principals, p)
	return h.receipt(), nil
}

func (h *gatewaySpikeHost) receipt() ActionHostResult {
	return ActionHostResult{Status: "verified", ApprovalID: h.beginResult.ApprovalID, ChangeID: h.beginResult.ChangeID,
		Card: NewCard().Title("Verified receipt", "green").
			Markdown("Acme fixture: added 1 follow-up; current next action: send fictional proposal. Operation: chg_fixture_A.").Build()}
}

func TestCUJ_SPIKE1_HostCardAndResultQueryPreserveConversation(t *testing.T) {
	p := &gatewaySpikePlatform{hostedCardPlatform: hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
	host := &gatewaySpikeHost{actionHostStub: actionHostStub{beginResult: ActionHostResult{
		Kind: "test.action.v1", Status: "pending", ApprovalID: "apr_fixture_A", ChangeID: "chg_fixture_A",
		Card: NewCard().Title("Approve Acme follow-up", "blue").
			Markdown("Current: first contact. Add: discussed fictional proposal at 10:00. Next action: send fictional proposal.").
			Buttons(PrimaryBtn("Approve", "approve:apr_fixture_A"), DefaultBtn("Modify", "modify:apr_fixture_A"), DangerBtn("Cancel", "cancel:apr_fixture_A")).Build(),
	}}}
	agent := &gatewaySpikeAgent{}
	e := NewEngine("test-project", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetActionHost(host)
	const key = "test:chat:owner"
	var modelToolResults []string // written by Send, read only after final turn.

	// Deliberately a test-only in-process seam. A production child cannot look
	// up Engine state; it needs scoped authenticated IPC and bounded read APIs.
	agent.tool = func(prompt string) string {
		var output string
		switch {
		case strings.Contains(prompt, "Read Acme history"):
			output = "CRM read: Acme fixture; history: first contact; next action: none."
		case strings.Contains(prompt, "Record a follow-up"):
			output = "Clarification: what time did the conversation occur? No CRM write."
		case strings.Contains(prompt, "It happened at 10:00"):
			e.interactiveMu.Lock()
			state := e.interactiveStates[key]
			e.interactiveMu.Unlock()
			h, staged, principal, ok := e.prepareHostedAction(state, ActionRef{Kind: host.Kind(), ChangeID: "chg_fixture_A"})
			if !ok {
				return "SPIKE_FAILED_TO_STAGE"
			}
			if err := e.publishHostedAction(h, staged, principal, p, "reply"); err != nil {
				return "SPIKE_FAILED_TO_PUBLISH"
			}
			output = "Staged; waiting for the original card. Canonical preview: " + staged.Card.RenderText()
		case strings.Contains(prompt, "Read an unrelated document"):
			output = "Other Feishu read: fictional document is available. Approval remains pending."
		case strings.Contains(prompt, "Show the operation result"):
			host.mu.Lock()
			if len(host.executions) == 1 {
				output = host.receipt().Card.RenderText()
			} else {
				output = "Result query: pending, not executed."
			}
			host.mu.Unlock()
		case strings.Contains(prompt, "Discuss the next step"):
			// No new CRM write or stage, just a read of the actual fixed receipt.
			host.mu.Lock()
			if len(host.executions) == 1 {
				output = "Based on the verified result, discuss sending the fictional proposal. No additional write."
			}
			host.mu.Unlock()
		}
		modelToolResults = append(modelToolResults, output)
		return output
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	if p.callback == nil {
		t.Fatal("real Engine did not register the trusted card callback")
	}
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine, agent: &agent.cujAgent}
	turn := 0
	send := func(text, expected string) {
		t.Helper()
		p.clearSent()
		turn++
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", ChannelID: "chat", UserID: "owner", UserName: "Fixture Owner",
			MessageID: fmt.Sprintf("fixture-message-%d", turn), Content: text, ReplyCtx: "reply"})
		env.waitFor(text, 2*time.Second, func() bool { return env.sentContains(expected) })
		if output := strings.Join(p.getSent(), "\n"); strings.Contains(output, "SPIKE_INTERNAL_") {
			t.Fatalf("Quiet leaked ordinary tool/thinking events: %s", output)
		}
	}
	send("/quiet quiet", "Quiet mode")
	send("Read Acme history", "history: first contact")
	send("Record a follow-up about the fictional proposal", "what time")
	send("It happened at 10:00", "Canonical preview")
	if !env.sentContains("Approve Acme follow-up") || !env.sentContains("Current: first contact") {
		t.Fatal("host canonical preview was not visible in Quiet mode")
	}
	send("Read an unrelated document", "fictional document is available")
	send("Show the operation result before approval", "pending, not executed")

	// Platform callbacks supply user/chat/message; card routes cannot substitute
	// another sender. Signature/OpenID extraction itself needs Feishu tests.
	click := TrustedCardAction{Kind: host.Kind(), ApprovalID: "apr_fixture_A", Decision: ActionApprove,
		Principal: ActionPrincipal{UserID: "intruder", ChatID: "chat", SessionKey: key, MessageID: "outgoing-approval-card"}}
	denied := p.callback(click)
	if denied.ToastType != "error" || denied.Card != nil || denied.Complete != nil {
		t.Fatalf("wrong-user callback = %#v", denied)
	}
	click.Principal.UserID = "owner"
	approved := p.callback(click)
	if approved.Complete == nil || approved.Card == nil || !strings.Contains(approved.Card.RenderText(), "Executing original plan") {
		t.Fatalf("approve callback = %#v", approved)
	}
	_ = p.RefreshCardMessage(e.ctx, click.Principal.MessageID, key, approved.Card)
	completed := approved.Complete()
	if completed.Card == nil {
		t.Fatal("host execution did not return a receipt card")
	}
	_ = p.RefreshCardMessage(e.ctx, click.Principal.MessageID, key, completed.Card)
	if !env.sentContains("added 1 follow-up") || !env.sentContains("chg_fixture_A") {
		t.Fatal("the original card did not expose the original action receipt")
	}
	duplicate := p.callback(click)
	if duplicate.Complete != nil || duplicate.Card == nil || !strings.Contains(duplicate.Card.RenderText(), "Verified receipt") {
		t.Fatalf("duplicate callback = %#v", duplicate)
	}
	send("Show the operation result", "current next action: send fictional proposal")
	send("Discuss the next step", "No additional write")

	host.mu.Lock()
	stages, executions := len(host.beginTokens), len(host.executions)
	principal, token := host.beginPrincipal, host.beginToken
	host.mu.Unlock()
	if stages != 1 || executions != 1 || token == "" || principal.UserID != "owner" || principal.ChatID != "chat" {
		t.Fatalf("stage/execute binding: stages=%d executions=%d principal=%#v", stages, executions, principal)
	}
	agent.mu.Lock()
	starts := len(agent.sessions)
	agent.mu.Unlock()
	e.interactiveMu.Lock()
	active := e.interactiveStates[key].agentSession.(*gatewaySpikeSession)
	e.interactiveMu.Unlock()
	if starts != 1 || !active.Alive() || active.permissions.Load() != 0 || len(active.getSentPrompts()) != 7 {
		t.Fatalf("session continuity: starts=%d alive=%v permissions=%d turns=%d", starts, active.Alive(), active.permissions.Load(), len(active.getSentPrompts()))
	}
	history := e.sessions.GetOrCreateActive(key).GetHistory(50)
	if len(history) < 14 || len(modelToolResults) != 7 || !strings.Contains(modelToolResults[5], "Verified receipt") {
		t.Fatalf("result/discussion missing from model or stored conversation: history=%d model-results=%#v", len(history), modelToolResults)
	}
	for _, entry := range history {
		if strings.Contains(entry.Content, "gateway host") {
			t.Fatal("legacy deny/terminate handoff note entered normal conversation")
		}
	}
}
