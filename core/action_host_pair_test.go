package core

import (
	"fmt"
	"testing"
)

type secondToolHost struct{ actionToolHostStub }

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
