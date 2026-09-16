package core

import (
	"fmt"
	"testing"
)

type secondToolHost struct{ actionToolHostStub }

type thirdToolHost struct{ actionToolHostStub }

func (h *thirdToolHost) Kind() string               { return "reminder.action.v1" }
func (h *thirdToolHost) RequiresMessageClock() bool { return true }

func TestThreeActionDomainsTraverseNestedToolsAndCards(t *testing.T) {
	e, primary, _, state := actionToolFixture(t)
	secondary, third := &secondToolHost{}, &thirdToolHost{}
	secondary.toolResult = map[string]any{"status": "ok"}
	third.toolResult = map[string]any{"status": "ok"}
	h := CombineActionHosts(CombineActionHosts(primary, secondary, []string{"knowledge_catalog", "knowledge_read"}), third, []string{"reminder-list"})
	e.SetActionHost(h)
	for _, command := range []string{"customer", "knowledge_catalog", "knowledge_read", "reminder-list"} {
		w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, fmt.Sprintf(`{"command":%q,"input":{}}`, command))
		if w.Code != 200 {
			t.Fatalf("%s: %s", command, w.Body.String())
		}
	}
	if primary.toolCalls != 1 || secondary.toolCalls != 2 || third.toolCalls != 1 {
		t.Fatal("cross-domain tool dispatch")
	}
	for _, leaf := range []ActionHost{primary, secondary, third} {
		if actionHostForKind(h, leaf.Kind()) != leaf {
			t.Fatal("nested card domain lost")
		}
	}
}

func TestActionCompositionRejectsNestedDuplicates(t *testing.T) {
	_, primary, _, _ := actionToolFixture(t)
	for _, h := range []ActionHost{
		CombineActionHosts(CombineActionHosts(primary, &secondToolHost{}, []string{"knowledge_read"}), &secondToolHost{}, []string{"reminder-list"}),
		CombineActionHosts(CombineActionHosts(primary, &secondToolHost{}, []string{"knowledge_read"}), &thirdToolHost{}, []string{"knowledge_read"}),
		CombineActionHosts(primary, &thirdToolHost{}, []string{"customer"}),
	} {
		if ValidateActionHosts(h) == nil || actionHostForCommand(h, "customer") != nil || actionHostForKind(h, primary.Kind()) != nil {
			t.Fatal("ambiguous routing accepted")
		}
		if _, err := h.SessionEnv("token"); err == nil {
			t.Fatal("ambiguous session accepted")
		}
	}
}

func (h *secondToolHost) Kind() string                  { return "second.action.v1" }
func (h *secondToolHost) Match(Event) (ActionRef, bool) { return ActionRef{}, false }

func TestActionHostPairRoutesToolsAndExactCardToTheirOwnHost(t *testing.T) {
	e, primary, platform, state := actionToolFixture(t)
	secondary := &secondToolHost{}
	secondary.toolResult = map[string]any{"status": "pending"}
	secondary.toolCard = &ActionHostResult{Kind: secondary.Kind(), Status: "pending", ApprovalID: "knowledge-approval",
		ChangeID: "knowledge-change", Card: NewCard().Title("Knowledge preview", "blue").Build()}
	e.SetActionHost(CombineActionHosts(primary, secondary, []string{"knowledge_propose", "knowledge_search", "knowledge_read"}))
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"knowledge_propose","input":{}}`)
	if w.Code != 200 || secondary.toolCalls != 1 || primary.toolCalls != 0 || len(secondary.cardBindings) != 1 || len(primary.cardBindings) != 0 {
		t.Fatalf("wrong proposal routing: %d %s", w.Code, w.Body.String())
	}
	if secondary.cardBindings[0].MessageID != "outgoing-approval-card" {
		t.Fatal("lost exact card binding")
	}
	callback := TrustedCardAction{Kind: secondary.Kind(), ApprovalID: "knowledge-approval", Decision: ActionApprove,
		Principal: state.currentPrincipal, Language: LangEnglish}
	callback.Principal.MessageID = "outgoing-approval-card"
	response := e.handleTrustedCardAction(platform, callback)
	if response.Complete == nil {
		t.Fatal("secondary callback did not claim")
	}
	response.Complete()
	if len(secondary.executions) != 1 || len(primary.executions) != 0 {
		t.Fatal("callback crossed action domain")
	}
	w = actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"customer","input":{}}`)
	if w.Code != 200 || primary.toolCalls != 1 || secondary.toolCalls != 1 {
		t.Fatal("existing tool changed routing")
	}
	if actionHostForKind(e.actionHost, "unknown") != nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestActionToolsLinkedReminderApprovalUsesCRMHostAndLegacyEmptyKind(t *testing.T) {
	for _, variant := range []string{"crm", "legacy-empty", "unknown"} {
		t.Run(variant, func(t *testing.T) {
			e, crm, platform, state := actionToolFixture(t)
			reminder := &thirdToolHost{}
			kind := crm.Kind()
			switch variant {
			case "legacy-empty":
				kind = ""
			case "unknown":
				kind = "unregistered.action"
			}
			reminder.toolResult = map[string]any{"status": "pending"}
			reminder.toolCard = &ActionHostResult{Kind: kind, Status: "pending", ApprovalID: "plan-approval", ChangeID: "plan-change", Card: NewCard().Title("CRM plan review", "blue").Build()}
			e.SetActionHost(CombineActionHosts(crm, reminder, []string{"reminder-update"}))
			w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", state.actionToken, `{"command":"reminder-update","input":{}}`)
			if reminder.toolCalls != 1 || crm.toolCalls != 0 {
				t.Fatal("linked edit did not enter reminder tool")
			}
			if variant == "unknown" {
				if w.Code != 503 || len(crm.cardBindings)+len(reminder.cardBindings) != 0 {
					t.Fatal("unknown approval domain was published")
				}
				return
			}
			selected, untouched := &crm.actionHostStub, &reminder.actionHostStub
			if variant == "legacy-empty" {
				selected, untouched = untouched, selected
				kind = reminder.Kind()
			}
			if w.Code != 200 || len(selected.cardBindings) != 1 || len(untouched.cardBindings) != 0 {
				t.Fatalf("wrong publication host: %d %s", w.Code, w.Body.String())
			}
			callback := TrustedCardAction{Kind: kind, ApprovalID: "plan-approval", Decision: ActionApprove, Principal: state.currentPrincipal, Language: LangEnglish}
			callback.Principal.MessageID = "outgoing-approval-card"
			response := e.handleTrustedCardAction(platform, callback)
			if response.Complete == nil {
				t.Fatal("approval callback lost original host")
			}
			response.Complete()
			if len(selected.executions) != 1 || len(untouched.executions) != 0 {
				t.Fatal("approval execution crossed domains")
			}
		})
	}
}

func TestActionToolsSixEnvironmentsRejectReboundAndAmbiguousTokens(t *testing.T) {
	var engines []*Engine
	var hosts []*actionToolHostStub
	var states []*interactiveState
	for index := 0; index < 6; index++ {
		e, h, _, state := actionToolFixture(t)
		e.name = fmt.Sprintf("environment-%d", index)
		state.currentPrincipal.Project, state.actionPrincipal.Project = e.name, e.name
		state.currentPrincipal.UserID = fmt.Sprintf("member-%d", index/2)
		state.actionPrincipal.UserID = state.currentPrincipal.UserID
		state.actionToken = fmt.Sprintf("session-%d", index)
		engines, hosts, states = append(engines, e), append(hosts, h), append(states, state)
	}
	handler := ActionToolsHandler(engines...)
	for index, state := range states {
		w := actionToolRequest(handler, "POST", "/tool", state.actionToken, `{"command":"customer","input":{}}`)
		if w.Code != 200 || hosts[index].toolPrincipal != state.currentPrincipal || hosts[index].toolCalls != 1 {
			t.Fatalf("environment %d lost identity", index)
		}
	}
	states[1].actionToken = states[0].actionToken
	if actionToolRequest(handler, "POST", "/tool", states[0].actionToken, `{"command":"customer","input":{}}`).Code != 401 {
		t.Fatal("ambiguous token accepted")
	}
	states[1].actionToken = "session-1"
	states[0].currentPrincipal.UserID = "another-member"
	if actionToolRequest(handler, "POST", "/tool", states[0].actionToken, `{"command":"customer","input":{}}`).Code != 401 {
		t.Fatal("rebound member accepted")
	}
}
