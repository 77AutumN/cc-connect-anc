package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type actionHostStub struct {
	mu             sync.Mutex
	decisions      []string
	principals     []ActionPrincipal
	envTokens      []string
	beginTokens    []string
	beginSessions  []string
	beginResult    ActionHostResult
	beginRef       ActionRef
	beginPrincipal ActionPrincipal
	beginToken     string
	executions     []string
	cardBindings   []ActionPrincipal
	bindErr        error
	claimResult    *ActionHostResult
	claimExecute   bool
	executeResult  *ActionHostResult
}

func (h *actionHostStub) BindCard(_ context.Context, approvalID string, principal ActionPrincipal) error {
	h.mu.Lock()
	h.cardBindings = append(h.cardBindings, principal)
	h.decisions = append(h.decisions, approvalID+":bind-card")
	h.mu.Unlock()
	return h.bindErr
}

func (h *actionHostStub) Kind() string { return "test.action.v1" }
func (h *actionHostStub) Match(event Event) (ActionRef, bool) {
	if event.Type != EventPermissionRequest || event.ToolName != "Bash" {
		return ActionRef{}, false
	}
	return ActionRef{Kind: h.Kind(), ChangeID: "chg_Abcdefgh12345678"}, true
}
func (h *actionHostStub) SessionEnv(token string) ([]string, error) {
	h.mu.Lock()
	h.envTokens = append(h.envTokens, token)
	h.mu.Unlock()
	return []string{"TEST_ACTION_TOKEN=" + token}, nil
}
func (h *actionHostStub) Begin(_ context.Context, ref ActionRef, principal ActionPrincipal, token string, _ Language) (ActionHostResult, error) {
	h.mu.Lock()
	h.beginRef = ref
	h.beginPrincipal = principal
	h.beginToken = token
	h.beginTokens = append(h.beginTokens, token)
	h.beginSessions = append(h.beginSessions, principal.SessionKey)
	result := h.beginResult
	h.mu.Unlock()
	return result, nil
}

func TestHostedActionTokenUniquePerLiveAgentAndSessionsRemainBound(t *testing.T) {
	agent := &hostedHandoffAgent{started: make(chan *hostedHandoffSession, 3)}
	platform := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test-platform"}}
	host := &actionHostStub{beginResult: ActionHostResult{
		Kind: "test.action.v1", Status: "pending", ApprovalID: "apr_visible",
		ChangeID: "chg_Abcdefgh12345678", Card: NewCard().Title("Approval pending", "blue").Build(),
	}}
	engine := NewEngine("test-project", agent, []Platform{platform}, "", LangEnglish)
	engine.SetActionHost(host)

	sendAndWait := func(sessionKey, messageID string) {
		t.Helper()
		engine.ReceiveMessage(platform, &Message{
			SessionKey: sessionKey, Platform: "test-platform", MessageID: messageID,
			ChannelID: "chat", UserID: "owner", Content: "stage action", ReplyCtx: "reply",
		})
		var session *hostedHandoffSession
		select {
		case session = <-agent.started:
		case <-time.After(2 * time.Second):
			t.Fatal("agent session did not start")
		}
		select {
		case <-session.permissions:
		case <-time.After(2 * time.Second):
			t.Fatal("host handoff did not complete")
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			stored := engine.sessions.GetOrCreateActive(sessionKey)
			if stored.TryLock() {
				stored.Unlock()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("hosted turn did not release its session lock")
			}
			time.Sleep(time.Millisecond)
		}
	}

	sendAndWait("test:chat:owner-a", "message-a1")
	sendAndWait("test:chat:owner-a", "message-a2")
	sendAndWait("test:chat:owner-b", "message-b1")

	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.envTokens) != 3 || host.envTokens[0] == "" || host.envTokens[1] == "" || host.envTokens[2] == "" ||
		host.envTokens[0] == host.envTokens[1] || host.envTokens[0] == host.envTokens[2] || host.envTokens[1] == host.envTokens[2] {
		t.Fatalf("session environment tokens = %#v", host.envTokens)
	}
	if len(host.beginTokens) != 3 || host.beginTokens[0] == host.beginTokens[1] || host.beginTokens[0] == host.beginTokens[2] || host.beginTokens[1] == host.beginTokens[2] {
		t.Fatalf("begin tokens were not unique by live agent session: %#v", host.beginTokens)
	}
	if len(host.beginSessions) != 3 || host.beginSessions[0] != host.beginSessions[1] || host.beginSessions[2] == host.beginSessions[0] {
		t.Fatalf("begin session binding = %#v", host.beginSessions)
	}
}
func (h *actionHostStub) Claim(_ context.Context, approvalID string, decision ActionDecision, principal ActionPrincipal, _ Language) (ActionHostResult, bool, error) {
	h.mu.Lock()
	h.decisions = append(h.decisions, approvalID+":"+string(decision))
	h.principals = append(h.principals, principal)
	h.mu.Unlock()
	if h.claimResult != nil {
		return *h.claimResult, h.claimExecute, nil
	}
	return ActionHostResult{Status: "executing", Card: NewCard().Title("executing", "blue").Build()}, decision == ActionApprove, nil
}
func (h *actionHostStub) Execute(_ context.Context, approvalID string, principal ActionPrincipal, _ Language) (ActionHostResult, error) {
	h.mu.Lock()
	h.executions = append(h.executions, approvalID)
	h.principals = append(h.principals, principal)
	h.mu.Unlock()
	if h.executeResult != nil {
		return *h.executeResult, nil
	}
	return ActionHostResult{Status: "verified", Card: NewCard().Title(approvalID, "green").Build()}, nil
}

type hostedHandoffAgent struct {
	mu              sync.Mutex
	env             []string
	session         *hostedHandoffSession
	started         chan *hostedHandoffSession
	respondErr      error
	respondErrAt    int
	extraPermission bool
}

func (a *hostedHandoffAgent) Name() string { return "hosted-test" }
func (a *hostedHandoffAgent) SetSessionEnv(env []string) {
	a.mu.Lock()
	a.env = append([]string(nil), env...)
	a.mu.Unlock()
}
func (a *hostedHandoffAgent) StartSession(context.Context, string) (AgentSession, error) {
	a.session = &hostedHandoffSession{events: make(chan Event, 5), permissions: make(chan PermissionResult, 3), respondErr: a.respondErr, respondErrAt: a.respondErrAt, extraPermission: a.extraPermission}
	if a.started != nil {
		a.started <- a.session
	}
	return a.session, nil
}
func (a *hostedHandoffAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *hostedHandoffAgent) Stop() error { return nil }

type hostedHandoffSession struct {
	events          chan Event
	permissions     chan PermissionResult
	mu              sync.Mutex
	sends           int
	respondErr      error
	respondErrAt    int
	responseCount   int
	extraPermission bool
	closed          bool
}

func (s *hostedHandoffSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	s.mu.Lock()
	s.sends++
	s.mu.Unlock()
	s.events <- Event{
		Type:      EventPermissionRequest,
		ToolName:  "Bash",
		RequestID: "req-handoff",
		ToolInputRaw: map[string]any{
			"command": "adapter-owned marker",
		},
	}
	s.events <- Event{Type: EventText, Content: "AGENT_TEXT_AFTER_HANDOFF_MUST_NOT_LEAK"}
	if s.extraPermission {
		s.events <- Event{Type: EventPermissionRequest, ToolName: "Bash", RequestID: "req-after-handoff", ToolInputRaw: map[string]any{"command": "unexpected"}}
	}
	s.events <- Event{Type: EventResult, Content: "AGENT_RESULT_AFTER_HANDOFF_MUST_NOT_LEAK", Done: true}
	return nil
}
func (s *hostedHandoffSession) RespondPermission(_ string, result PermissionResult) error {
	s.permissions <- result
	s.mu.Lock()
	s.responseCount++
	count := s.responseCount
	s.mu.Unlock()
	if s.respondErr != nil && (s.respondErrAt == 0 || s.respondErrAt == count) {
		return s.respondErr
	}
	return nil
}
func (s *hostedHandoffSession) Events() <-chan Event     { return s.events }
func (s *hostedHandoffSession) CurrentSessionID() string { return "hosted-agent-session" }
func (s *hostedHandoffSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}
func (s *hostedHandoffSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type hostedCardPlatform struct {
	stubPlatformEngine
	cardMu       sync.Mutex
	cards        []*Card
	placeholders []*Card
	refreshes    []*Card
	publishErr   error
}

func (p *hostedCardPlatform) ReplyCard(ctx context.Context, replyCtx any, card *Card) error {
	p.cardMu.Lock()
	p.cards = append(p.cards, card)
	p.cardMu.Unlock()
	return p.Reply(ctx, replyCtx, card.RenderText())
}
func (p *hostedCardPlatform) SendCard(ctx context.Context, replyCtx any, card *Card) error {
	return p.ReplyCard(ctx, replyCtx, card)
}
func (p *hostedCardPlatform) ReplyHostedActionPlaceholder(_ context.Context, _ any, card *Card) (string, error) {
	if p.publishErr != nil {
		return "", p.publishErr
	}
	p.cardMu.Lock()
	p.placeholders = append(p.placeholders, card)
	p.cards = append(p.cards, card)
	p.cardMu.Unlock()
	return "outgoing-approval-card", nil
}

func TestHostedActionDoesNotPublishWhenAgentDenyFails(t *testing.T) {
	agent := &hostedHandoffAgent{started: make(chan *hostedHandoffSession, 1), respondErr: errors.New("stdin closed")}
	platform := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test-platform"}}
	host := &actionHostStub{beginResult: ActionHostResult{
		Kind: "test.action.v1", Status: "pending", ApprovalID: "apr_handoff",
		ChangeID: "chg_Abcdefgh12345678", Card: NewCard().Title("Approval pending", "blue").Build(),
	}}
	engine := NewEngine("test-project", agent, []Platform{platform}, "", LangEnglish)
	engine.SetActionHost(host)
	engine.ReceiveMessage(platform, &Message{
		SessionKey: "test:chat:owner", Platform: "test-platform", MessageID: "request",
		ChannelID: "chat", UserID: "owner", Content: "record action", ReplyCtx: "reply",
	})

	var session *hostedHandoffSession
	select {
	case session = <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent session did not start")
	}
	select {
	case <-session.permissions:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not attempt to deny the marker")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.mu.Lock()
		closed := session.closed
		session.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent session was not closed after deny failure")
		}
		time.Sleep(time.Millisecond)
	}
	platform.cardMu.Lock()
	placeholderCount := len(platform.placeholders)
	refreshCount := len(platform.refreshes)
	platform.cardMu.Unlock()
	host.mu.Lock()
	bindCount := len(host.cardBindings)
	host.mu.Unlock()
	if placeholderCount != 0 || refreshCount != 0 || bindCount != 0 {
		t.Fatalf("approval became actionable after deny failure: placeholders=%d refreshes=%d bindings=%d", placeholderCount, refreshCount, bindCount)
	}
}

func TestHostedActionClosesAgentBeforeLaterEventsCanRun(t *testing.T) {
	agent := &hostedHandoffAgent{
		started: make(chan *hostedHandoffSession, 1), respondErr: errors.New("stdin closed"),
		respondErrAt: 2, extraPermission: true,
	}
	platform := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test-platform"}}
	host := &actionHostStub{beginResult: ActionHostResult{
		Kind: "test.action.v1", Status: "pending", ApprovalID: "apr_handoff",
		ChangeID: "chg_Abcdefgh12345678", Card: NewCard().Title("Approval pending", "blue").Build(),
	}}
	engine := NewEngine("test-project", agent, []Platform{platform}, "", LangEnglish)
	engine.SetActionHost(host)
	engine.ReceiveMessage(platform, &Message{
		SessionKey: "test:chat:owner", Platform: "test-platform", MessageID: "request",
		ChannelID: "chat", UserID: "owner", Content: "record action", ReplyCtx: "reply",
	})
	var session *hostedHandoffSession
	select {
	case session = <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent session did not start")
	}
	select {
	case permission := <-session.permissions:
		if permission.Behavior != "deny" {
			t.Fatalf("permission = %#v", permission)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("marker denial was not observed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.mu.Lock()
		closed := session.closed
		session.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent session was not closed after the later deny failed")
		}
		time.Sleep(time.Millisecond)
	}
	var placeholderCount, refreshCount int
	deadline = time.Now().Add(2 * time.Second)
	for {
		platform.cardMu.Lock()
		placeholderCount = len(platform.placeholders)
		refreshCount = len(platform.refreshes)
		platform.cardMu.Unlock()
		if placeholderCount == 1 && refreshCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if placeholderCount != 1 || refreshCount != 1 {
		t.Fatalf("the already host-owned approval was not published exactly once: placeholders=%d refreshes=%d", placeholderCount, refreshCount)
	}
	select {
	case unexpected := <-session.permissions:
		t.Fatalf("post-marker permission was processed after close: %#v", unexpected)
	default:
	}
}

func TestHostedActionPlaceholderFailureNeverBindsOrActivatesApproval(t *testing.T) {
	agent := &hostedHandoffAgent{started: make(chan *hostedHandoffSession, 1)}
	platform := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test-platform"}, publishErr: errors.New("send failed")}
	host := &actionHostStub{beginResult: ActionHostResult{
		Kind: "test.action.v1", Status: "pending", ApprovalID: "apr_handoff",
		ChangeID: "chg_Abcdefgh12345678", Card: NewCard().Title("Approval pending", "blue").Build(),
	}}
	engine := NewEngine("test-project", agent, []Platform{platform}, "", LangEnglish)
	engine.SetActionHost(host)
	engine.ReceiveMessage(platform, &Message{
		SessionKey: "test:chat:owner", Platform: "test-platform", MessageID: "request",
		ChannelID: "chat", UserID: "owner", Content: "record action", ReplyCtx: "reply",
	})
	var session *hostedHandoffSession
	select {
	case session = <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent session did not start")
	}
	select {
	case permission := <-session.permissions:
		if permission.Behavior != "deny" {
			t.Fatalf("permission = %#v", permission)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not deny the marker")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		host.mu.Lock()
		bindCount := len(host.cardBindings)
		host.mu.Unlock()
		platform.cardMu.Lock()
		refreshCount := len(platform.refreshes)
		platform.cardMu.Unlock()
		if bindCount != 0 || refreshCount != 0 {
			t.Fatalf("unpublished approval was bound or activated: bindings=%d refreshes=%d", bindCount, refreshCount)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHostedActionBindingFailureLeavesOnlyNonActionablePlaceholder(t *testing.T) {
	agent := &hostedHandoffAgent{started: make(chan *hostedHandoffSession, 1)}
	platform := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test-platform"}}
	host := &actionHostStub{bindErr: errors.New("ledger unavailable"), beginResult: ActionHostResult{
		Kind: "test.action.v1", Status: "pending", ApprovalID: "apr_handoff",
		ChangeID: "chg_Abcdefgh12345678", Card: NewCard().Title("Approval pending", "blue").Build(),
	}}
	engine := NewEngine("test-project", agent, []Platform{platform}, "", LangEnglish)
	engine.SetActionHost(host)
	engine.ReceiveMessage(platform, &Message{
		SessionKey: "test:chat:owner", Platform: "test-platform", MessageID: "request",
		ChannelID: "chat", UserID: "owner", Content: "record action", ReplyCtx: "reply",
	})
	var session *hostedHandoffSession
	select {
	case session = <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent session did not start")
	}
	select {
	case <-session.permissions:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not deny the marker")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		platform.cardMu.Lock()
		placeholderCount := len(platform.placeholders)
		refreshes := append([]*Card(nil), platform.refreshes...)
		platform.cardMu.Unlock()
		if placeholderCount == 1 && len(refreshes) == 1 {
			if refreshes[0].Header == nil || refreshes[0].Header.Title != NewI18n(LangEnglish).T(MsgHostedActionFailedTitle) {
				t.Fatalf("binding failure activated the business card: %#v", refreshes[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("placeholder/failure refresh not observed: placeholders=%d refreshes=%d", placeholderCount, len(refreshes))
		}
		time.Sleep(time.Millisecond)
	}
}
func (p *hostedCardPlatform) RefreshCardMessage(_ context.Context, messageID, _ string, card *Card) error {
	if messageID != "outgoing-approval-card" {
		return context.Canceled
	}
	p.cardMu.Lock()
	p.refreshes = append(p.refreshes, card)
	if len(p.cards) > 0 {
		p.cards[len(p.cards)-1] = card
	}
	p.cardMu.Unlock()
	return nil
}

func TestHostedActionHandoffDeniesAgentAndSuppressesRemainingTurn(t *testing.T) {
	agent := &hostedHandoffAgent{started: make(chan *hostedHandoffSession, 1)}
	platform := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test-platform"}}
	host := &actionHostStub{beginResult: ActionHostResult{
		Kind:       "test.action.v1",
		Status:     "pending",
		ApprovalID: "apr_handoff",
		ChangeID:   "chg_Abcdefgh12345678",
		Card:       NewCard().Title("Approval pending", "blue").Build(),
	}}
	engine := NewEngine("test-project", agent, []Platform{platform}, "", LangEnglish)
	engine.SetActionHost(host)

	engine.ReceiveMessage(platform, &Message{
		SessionKey: "test:chat:owner",
		Platform:   "test-platform",
		MessageID:  "message-request",
		ChannelID:  "chat",
		UserID:     "owner",
		Content:    "record action",
		ReplyCtx:   "reply",
	})

	var agentSession *hostedHandoffSession
	select {
	case agentSession = <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("agent session did not start")
	}
	var permission PermissionResult
	select {
	case permission = <-agentSession.permissions:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not resolve marker PermissionRequest")
	}
	if permission.Behavior != "deny" || !strings.Contains(permission.Message, "gateway host") {
		t.Fatalf("handoff permission = %#v", permission)
	}

	host.mu.Lock()
	beginToken := host.beginToken
	beginPrincipal := host.beginPrincipal
	host.mu.Unlock()
	if beginToken == "" {
		t.Fatal("host begin did not receive the live-session action token")
	}
	if beginPrincipal.UserID != "owner" || beginPrincipal.ChatID != "chat" ||
		beginPrincipal.SessionKey != "test:chat:owner" || beginPrincipal.Project != "test-project" {
		t.Fatalf("host begin principal = %#v", beginPrincipal)
	}
	agent.mu.Lock()
	var tokenInjected bool
	for _, entry := range agent.env {
		if strings.HasPrefix(entry, "TEST_ACTION_TOKEN=") && len(entry) > len("TEST_ACTION_TOKEN=") {
			tokenInjected = true
		}
	}
	agent.mu.Unlock()
	if !tokenInjected {
		t.Fatalf("agent env did not receive adapter-owned action token: %#v", agent.env)
	}

	platform.cardMu.Lock()
	cardCount := len(platform.cards)
	platform.cardMu.Unlock()
	if cardCount != 1 {
		t.Fatalf("business cards = %d, want exactly 1", cardCount)
	}
	host.mu.Lock()
	if len(host.cardBindings) != 1 || host.cardBindings[0].MessageID != "outgoing-approval-card" {
		host.mu.Unlock()
		t.Fatalf("emitted approval card was not bound: %#v", host.cardBindings)
	}
	host.mu.Unlock()
	for _, output := range platform.getSent() {
		if strings.Contains(output, "AGENT_") || strings.Contains(strings.ToLower(output), "allow all") {
			t.Fatalf("post-handoff or generic permission output leaked: %q", output)
		}
	}
	agentSession.mu.Lock()
	sends := agentSession.sends
	agentSession.mu.Unlock()
	if sends != 1 {
		t.Fatalf("agent was re-awakened after handoff: sends=%d", sends)
	}
}

func TestHostedActionCallbackDelegatesAuthorizationToPersistentHost(t *testing.T) {
	host := &actionHostStub{}
	p := &stubPlatformEngine{n: "test-platform"}
	e := NewEngine("test-project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetActionHost(host)
	response := e.handleTrustedCardAction(p, TrustedCardAction{
		Kind: "test.action.v1", ApprovalID: "apr_one", Decision: ActionApprove,
		Principal: ActionPrincipal{UserID: "owner", ChatID: "chat", SessionKey: "test:chat:owner", MessageID: "card-one"},
	})
	if response.Card == nil || response.Card.Header == nil || response.Card.Header.Title != "executing" || response.Complete == nil {
		t.Fatalf("decision response = %#v", response)
	}
	completed := response.Complete()
	if completed.Card == nil || completed.Card.Header == nil || completed.Card.Header.Title != "apr_one" {
		t.Fatalf("completion response = %#v", completed)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.principals) != 2 || host.principals[0].Platform != "test-platform" || host.principals[0].Project != "test-project" || len(host.executions) != 1 {
		t.Fatalf("persistent host did not receive trusted principal: %#v", host.principals)
	}
}

func TestHostedActionDuplicateExecutingCallbackIsToastOnly(t *testing.T) {
	stale := ActionHostResult{Status: "executing", Code: "apply_in_progress", Card: NewCard().Title("stale executing", "blue").Build()}
	host := &actionHostStub{claimResult: &stale, claimExecute: false}
	p := &stubPlatformEngine{n: "test-platform"}
	e := NewEngine("test-project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetActionHost(host)

	response := e.handleTrustedCardAction(p, TrustedCardAction{
		Kind: "test.action.v1", ApprovalID: "apr_one", Decision: ActionApprove,
		Principal: ActionPrincipal{UserID: "owner", ChatID: "chat", SessionKey: "test:chat:owner", MessageID: "card-one"},
	})
	if response.Card != nil || response.Complete != nil || response.Toast == "" {
		t.Fatalf("duplicate response = %#v, want toast-only response", response)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.executions) != 0 {
		t.Fatalf("duplicate callback executed host: %#v", host.executions)
	}
}

func TestHostedActionProjectRejectsRelayBeforeStartingAgent(t *testing.T) {
	agent := &hostedHandoffAgent{started: make(chan *hostedHandoffSession, 1)}
	engine := NewEngine("test-project", agent, nil, "", LangEnglish)
	engine.SetActionHost(&actionHostStub{})
	response, err := engine.HandleRelay(context.Background(), "other-project", "feishu:chat:user", "do work")
	if err == nil || response != "" || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("HandleRelay() = (%q, %v), want fail-closed", response, err)
	}
	select {
	case <-agent.started:
		t.Fatal("relay started an agent session in the action-host project")
	default:
	}
}
