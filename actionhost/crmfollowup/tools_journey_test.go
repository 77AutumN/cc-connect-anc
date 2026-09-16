package crmfollowup

// Opt-in cross-repository contract journey, not a live model/Feishu eval.
// Real: Engine, SessionManager, authenticated HTTP handler, Adapter subprocess,
// Python CRM core and persistent SQLite. Fake: agent intentions, platform I/O
// and the fixture Feishu backend. No production configuration is loaded.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type toolJourneyStep struct {
	number  int
	command string
	input   map[string]any
	plain   string
}

type toolJourneyReply struct {
	data map[string]any
	err  error
}

type toolJourneyAgent struct {
	url         string
	client      *http.Client
	steps       chan toolJourneyStep
	results     chan toolJourneyReply
	events      chan core.Event
	mu          sync.Mutex
	env         []string
	starts      atomic.Int32
	permissions atomic.Int32
	closes      atomic.Int32
	turns       atomic.Int32
}

func (*toolJourneyAgent) Name() string { return "scripted-offline-agent" }
func (a *toolJourneyAgent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.env = append([]string(nil), env...)
}
func (a *toolJourneyAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	a.starts.Add(1)
	return a, nil
}
func (*toolJourneyAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (*toolJourneyAgent) Stop() error { return nil }
func (a *toolJourneyAgent) Send(string, string, []core.ImageAttachment, []core.FileAttachment) error {
	step := <-a.steps
	a.turns.Add(1)
	data := map[string]any{"discussion": step.plain}
	var err error
	if step.command != "" {
		data = nil
		a.mu.Lock()
		var token string
		for _, item := range a.env {
			if value, found := strings.CutPrefix(item, actionTokenEnv+"="); found {
				token = value
			}
		}
		a.mu.Unlock()
		payload, _ := json.Marshal(map[string]any{"command": step.command, "input": step.input})
		request, requestErr := http.NewRequest(http.MethodPost, a.url+"/tool", bytes.NewReader(payload))
		if requestErr != nil {
			err = requestErr
		} else {
			request.Header.Set("Authorization", "Bearer "+token)
			var response *http.Response
			response, err = a.client.Do(request)
			if err == nil {
				defer func() { _ = response.Body.Close() }()
				err = json.NewDecoder(response.Body).Decode(&data)
				if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("tool HTTP status %d: %v", response.StatusCode, data)
				}
			}
		}
	}
	a.results <- toolJourneyReply{data: data, err: err}
	output, _ := json.Marshal(data)
	a.events <- core.Event{Type: core.EventThinking, Content: "INTERNAL_SPIKE_THINKING"}
	a.events <- core.Event{Type: core.EventToolUse, ToolName: "INTERNAL_SPIKE_TOOL"}
	a.events <- core.Event{Type: core.EventResult, Content: fmt.Sprintf("TURN-%d %s", step.number, output), Done: true}
	return nil
}
func (a *toolJourneyAgent) RespondPermission(string, core.PermissionResult) error {
	a.permissions.Add(1)
	return fmt.Errorf("the controlled tool path must not intercept permissions")
}
func (a *toolJourneyAgent) Events() <-chan core.Event { return a.events }
func (*toolJourneyAgent) CurrentSessionID() string    { return "offline-persistent-agent" }
func (a *toolJourneyAgent) Alive() bool               { return a.closes.Load() == 0 }
func (a *toolJourneyAgent) Close() error              { a.closes.Add(1); return nil }

type toolJourneyPlatform struct {
	mu         sync.Mutex
	sent       []string
	cards      map[string]*core.Card
	cardIDs    []string
	questionUI []string
	callback   core.TrustedCardActionHandler
}

func (*toolJourneyPlatform) Name() string                                { return "mock" }
func (*toolJourneyPlatform) Start(core.MessageHandler) error             { return nil }
func (*toolJourneyPlatform) Stop() error                                 { return nil }
func (*toolJourneyPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }
func (p *toolJourneyPlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *toolJourneyPlatform) Send(ctx context.Context, target any, content string) error {
	return p.Reply(ctx, target, content)
}
func (p *toolJourneyPlatform) ReplyCard(_ context.Context, _ any, card *core.Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, card.RenderText())
	if nativeQuestionCard(card) {
		p.questionUI = append(p.questionUI, card.RenderText())
	}
	return nil
}
func (p *toolJourneyPlatform) SendCard(ctx context.Context, target any, card *core.Card) error {
	return p.ReplyCard(ctx, target, card)
}

func nativeQuestionCard(card *core.Card) bool {
	if card == nil || card.Header == nil {
		return false
	}
	for _, element := range card.Elements {
		if item, ok := element.(core.CardListItem); ok && strings.HasPrefix(item.BtnValue, "askq:") &&
			item.Extra["askq_label"] != "" && item.Extra["askq_question"] != "" {
			return true
		}
	}
	// Native multi-select/free-text questions have no answer buttons.
	for _, lang := range []core.Language{core.LangEnglish, core.LangChinese, core.LangTraditionalChinese, core.LangJapanese, core.LangSpanish} {
		title := core.NewI18n(lang).T(core.MsgAskQuestionTitle)
		if !card.HasButtons() && card.Header.Color == "blue" && (card.Header.Title == title ||
			(strings.HasPrefix(card.Header.Title, title+" (") && strings.HasSuffix(card.Header.Title, ")"))) {
			return true
		}
	}
	return false
}

func (p *toolJourneyPlatform) SetTrustedCardActionHandler(h core.TrustedCardActionHandler) {
	p.callback = h
}
func (p *toolJourneyPlatform) ReplyHostedActionPlaceholder(_ context.Context, _ any, card *core.Card) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := fmt.Sprintf("fixture-card-%d", len(p.cardIDs)+1)
	p.cardIDs = append(p.cardIDs, id)
	p.cards[id] = card
	p.sent = append(p.sent, card.RenderText())
	return id, nil
}
func (p *toolJourneyPlatform) RefreshCardMessage(_ context.Context, id, _ string, card *core.Card) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, found := p.cards[id]; !found {
		return fmt.Errorf("cannot refresh an unknown card")
	}
	p.cards[id] = card
	p.sent = append(p.sent, card.RenderText())
	return nil
}
func (p *toolJourneyPlatform) transcript() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.sent, "\n")
}
func (p *toolJourneyPlatform) lastCard() (string, *core.Card) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cardIDs) == 0 {
		return "", nil
	}
	id := p.cardIDs[len(p.cardIDs)-1]
	return id, p.cards[id]
}

func (p *toolJourneyPlatform) hasNewQuestion(before int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.questionUI) > before
}

func TestCanaryNativeQuestionsAreFreshAndSeparateFromHostedCards(t *testing.T) {
	p := &toolJourneyPlatform{cards: map[string]*core.Card{}}
	sender, ok := any(p).(core.CardSender)
	if !ok {
		t.Fatal("canary platform cannot present native question cards")
	}
	question := core.NewCard().Title(core.NewI18n(core.LangEnglish).T(core.MsgAskQuestionTitle), "blue").
		ListItemBtnExtra("Existing customer", "Existing", "default", "askq:0:1", map[string]string{"askq_label": "Existing", "askq_question": "Which customer?"}).Build()
	if err := sender.SendCard(context.Background(), "group", question); err != nil || !p.hasNewQuestion(0) {
		t.Fatal("new native question was not detected")
	}
	before := len(p.questionUI)
	business := core.NewCard().Title("Approval required", "blue").Markdown("AskUserQuestion is only quoted data here").Buttons(core.PrimaryBtn("Approve", "approve")).Build()
	id, _ := p.ReplyHostedActionPlaceholder(context.Background(), "group", business)
	if err := p.RefreshCardMessage(context.Background(), id, "group", business); err != nil {
		t.Fatal(err)
	}
	if err := sender.ReplyCard(context.Background(), "group", business); err != nil || p.hasNewQuestion(before) {
		t.Fatal("old question or business card was treated as a new native question")
	}
	multi := core.NewCard().Title(core.NewI18n(core.LangEnglish).T(core.MsgAskQuestionTitle)+" (2/2)", "blue").Markdown("Select the relevant facts").Build()
	if err := sender.SendCard(context.Background(), "group", multi); err != nil || !p.hasNewQuestion(before) || len(p.cardIDs) != 1 {
		t.Fatal("native question header was lost or question UI changed hosted approval counts")
	}
	for name := range customerBehaviorCases {
		want := name == "customer-missing-input" || name == "customer-duplicate-ask" || name == "customer-duplicate-distinct" || name == "customer-assignee-unknown" || strings.HasPrefix(name, "customer-draft-")
		if canaryAllowsNativeQuestion(name, 1) != want || canaryAllowsNativeQuestion(name, 2) != (name == "customer-roster-ambiguous") || canaryAllowsNativeQuestion(name, 3) {
			t.Fatalf("native question ended an unexpected case/turn: %s", name)
		}
	}
}

type nativeQuestionJourney struct {
	toolJourneyAgent
	observed *realCanarySession
	answer   chan core.PermissionResult
}

func (a *nativeQuestionJourney) StartSession(ctx context.Context, _ string) (core.AgentSession, error) {
	a.starts.Add(1)
	a.observed.ctx = ctx
	return a.observed, nil
}
func (a *nativeQuestionJourney) Send(string, string, []core.ImageAttachment, []core.FileAttachment) error {
	a.events <- core.Event{Type: core.EventText, Content: "Required: contact, stage, owner and communication time."}
	a.events <- core.Event{Type: core.EventPermissionRequest, RequestID: "native-question", ToolName: "AskUserQuestion",
		ToolInputRaw: map[string]any{"questions": []any{map[string]any{"question": "Which customer?"}}},
		Questions:    []core.UserQuestion{{Question: "Which customer?", Options: []core.UserQuestionOption{{Label: "Existing"}, {Label: "Distinct"}}}}}
	return nil
}
func (a *nativeQuestionJourney) RespondPermission(id string, result core.PermissionResult) error {
	if id == "native-question" {
		a.answer <- result
		a.events <- core.Event{Type: core.EventResult, Content: "Distinct company acknowledged", Done: true}
	}
	return nil
}

func TestCanaryNativeQuestionAnswerUsesExistingSessionAndSeparatePermissionCount(t *testing.T) {
	p := &toolJourneyPlatform{cards: map[string]*core.Card{}}
	a := &nativeQuestionJourney{toolJourneyAgent: toolJourneyAgent{events: make(chan core.Event, 8)}, answer: make(chan core.PermissionResult, 1)}
	observed := &realCanaryAgent{}
	a.observed = &realCanarySession{AgentSession: a, agent: observed}
	e := core.NewEngine("test", a, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	const key = "mock:group-1:sender-1"
	receive := func(content string) {
		e.ReceiveMessage(p, &core.Message{Platform: "mock", SessionKey: key, UserID: "sender-1", ChannelID: "group-1", MessageID: content, Content: content, ReplyCtx: "group"})
	}
	receive("/quiet quiet")
	receive("Clarify customer")
	deadline := time.Now().Add(3 * time.Second)
	for !p.hasNewQuestion(0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !p.hasNewQuestion(0) || !e.GetSessions().GetOrCreateActive(key).Busy() || !strings.Contains(p.transcript(), "Required: contact, stage, owner and communication time.") {
		t.Fatal("native question did not retain its visible preface and waiting session")
	}
	events := a.observed.Events()
	if !a.observed.hasPendingQuestion() || events != a.observed.Events() {
		t.Fatal("question UI did not match a native request on the shared event channel")
	}
	const answer = "These are not the customer; create a distinct company with the earlier details."
	receive(answer)
	select {
	case result := <-a.answer:
		if result.Behavior != "allow" || result.UpdatedInput["answers"].(map[string]any)["Which customer?"] != answer {
			t.Fatal("ReceiveMessage did not return the natural answer to the native request")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native question was not answered through ReceiveMessage")
	}
	deadline = time.Now().Add(3 * time.Second)
	for e.GetSessions().GetOrCreateActive(key).Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if e.GetSessions().GetOrCreateActive(key).Busy() || a.observed.hasPendingQuestion() || a.starts.Load() != 1 || observed.sends.Load() != 1 || observed.questionAnswers.Load() != 1 || observed.permissions.Load() != 0 || len(p.cardIDs) != 0 {
		t.Fatal("answer changed native session, hosted-card or ordinary permission boundaries")
	}
	// An ordinary permission remains ordinary even if its input resembles answers.
	if err := a.observed.RespondPermission("ordinary-request", core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{"answers": map[string]any{"Question": "Answer"}}}); err != nil || observed.permissions.Load() != 1 || observed.questionAnswers.Load() != 1 {
		t.Fatal("ordinary permission was mistaken for a native question answer")
	}
}

func TestCUJ_CRMIPC1_CustomerApprovalAndContinuedDiscussion(t *testing.T) {
	crmRoot := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if crmRoot == "" {
		t.Skip("opt-in offline cross-repo journey: set MYANC_SPIKE_CRM_ROOT to the myanc-crm checkout")
	}
	fixture := filepath.Join(crmRoot, "spikes", "host_tool_fixture.py")
	if _, err := os.Stat(fixture); err != nil {
		t.Fatal(err)
	}
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	// Go's TempDir leaf uses 0777 filtered by umask; the real ledger requires 0700.
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	var executes atomic.Int32
	a := &Adapter{hostSecret: "offline-gateway-host-secret-00000000000001"}
	if err := a.EnableTools(); err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkDir(scratch, account.Username); err != nil {
		t.Fatal(err)
	}
	a.run = func(ctx context.Context, _ string, subcommand string, input []byte, env []string) ([]byte, error) {
		if subcommand == "host-execute" {
			executes.Add(1)
		}
		command := exec.CommandContext(ctx, python, "-B", fixture, subcommand)
		command.Env = append(env, "MYANC_SPIKE_DIR="+scratch)
		command.Stdin = bytes.NewReader(input)
		output, err := command.Output()
		if failure, ok := err.(*exec.ExitError); ok {
			t.Logf("offline fixture %s failed: stdout=%s stderr=%s", subcommand, output, failure.Stderr)
		}
		return output, err
	}
	p := &toolJourneyPlatform{cards: make(map[string]*core.Card)}
	agent := &toolJourneyAgent{client: &http.Client{Timeout: 10 * time.Second}, steps: make(chan toolJourneyStep, 1), results: make(chan toolJourneyReply, 1), events: make(chan core.Event, 16)}
	e := core.NewEngine("test", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	e.SetActionHost(a)
	server := httptest.NewServer(e.ActionToolHandler())
	agent.url = server.URL
	t.Cleanup(server.Close)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	if p.callback == nil {
		t.Fatal("real Engine did not install the trusted callback")
	}
	const key = "mock:group-1:sender-1"
	n := 0
	receive := func(content string) {
		e.ReceiveMessage(p, &core.Message{Platform: "mock", SessionKey: key, UserID: "sender-1", UserName: "Fictional Owner", ChannelID: "group-1", MessageID: fmt.Sprintf("inbound-%d", n), Content: content, ReplyCtx: "fixture-group"})
	}
	waitFor := func(fragment string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(p.transcript(), fragment) {
			if time.Now().After(deadline) {
				t.Fatalf("user never received %q; transcript: %s", fragment, p.transcript())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	receive("/quiet quiet")
	waitFor("Quiet mode")
	turn := func(description, command string, input map[string]any) map[string]any {
		t.Helper()
		n++
		agent.steps <- toolJourneyStep{number: n, command: command, input: input, plain: description}
		receive(description)
		var reply toolJourneyReply
		select {
		case reply = <-agent.results:
		case <-time.After(10 * time.Second):
			t.Fatalf("agent did not finish %q; transcript: %s", description, p.transcript())
		}
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		waitFor(fmt.Sprintf("TURN-%d ", n))
		return reply.data
	}
	expectStatus := func(result map[string]any, expected string) {
		t.Helper()
		if result["status"] != expected {
			t.Fatalf("wanted %s, got %v", expected, result)
		}
	}
	query := map[string]any{"customer_query": "C-001"}
	before := turn("Read the customer's history", "customer", query)
	expectStatus(before, "found")
	baselineHistory := len(before["history"].([]any))
	missing := turn("Record a follow-up; ask me for its time", "stage", map[string]any{"customer_query": "C-001", "content": "Fictional discussion"})
	expectStatus(missing, "needs_time")
	if id, _ := p.lastCard(); id != "" {
		t.Fatal("missing time created an approval card")
	}
	stage := func(content string) (map[string]any, string) {
		t.Helper()
		result := turn("Stage "+content, "stage", map[string]any{"customer_query": "C-001", "occurred_at": "2026-09-05T12:00:00+08:00", "channel": "飞书", "content": content, "next_action": "Send fictional proposal"})
		expectStatus(result, "pending")
		id, card := p.lastCard()
		if card == nil || !strings.Contains(card.RenderText(), content) || card.RenderText() != approvalCard(result, core.LangEnglish).RenderText() {
			t.Fatalf("model and host card did not receive the same canonical preview: %v", result)
		}
		return result, id
	}
	resultOf := func(staged map[string]any) map[string]any {
		return turn("Read the operation result without executing", "result", map[string]any{"change_id": staged["change_id"]})
	}
	click := func(staged map[string]any, cardID, user string, decision core.ActionDecision) core.TrustedCardActionResponse {
		t.Helper()
		response := p.callback(core.TrustedCardAction{Kind: Kind, ApprovalID: text(staged["approval_id"]), Decision: decision, Language: core.LangEnglish, Principal: core.ActionPrincipal{UserID: user, ChatID: "group-1", SessionKey: key, MessageID: cardID}})
		if response.Card != nil {
			if err := p.RefreshCardMessage(context.Background(), cardID, key, response.Card); err != nil {
				t.Fatal(err)
			}
		}
		return response
	}

	planA, cardA := stage("Original fictional plan A")
	// This proves ordinary turns still route; it does not exercise a real
	// non-CRM Feishu API, CLI hook, or credentials.
	turn("Discuss an unrelated document while approval waits", "", nil)
	expectStatus(resultOf(planA), "pending")
	modified := click(planA, cardA, "sender-1", core.ActionModify)
	if modified.Complete != nil {
		t.Fatal("modify attempted a CRM write")
	}
	expectStatus(resultOf(planA), "superseded")
	planB, cardB := stage("Revised fictional plan B")
	planC, cardC := stage("Replacement fictional plan C")
	old := click(planB, cardB, "sender-1", core.ActionApprove)
	if old.Complete != nil {
		t.Fatal("superseded approval remained executable")
	}
	expectStatus(resultOf(planB), "superseded")
	cancelled := click(planC, cardC, "sender-1", core.ActionCancel)
	if cancelled.Complete != nil {
		t.Fatal("cancel attempted a CRM write")
	}
	expectStatus(resultOf(planC), "cancelled")
	if executes.Load() != 0 {
		t.Fatal("CRM executed before approval")
	}

	planD, cardD := stage("Approved original fictional plan D")
	wrongUser := click(planD, cardD, "sender-2", core.ActionApprove)
	if wrongUser.ToastType != "error" || wrongUser.Card != nil || wrongUser.Complete != nil {
		t.Fatalf("wrong sender was not rejected: %#v", wrongUser)
	}
	// Race real host claims through separate Python processes / SQLite
	// connections. The serial JSON fake is not written until the sole winner
	// below executes, so this tests CAS without pretending it is a DB backend.
	claims := make(chan core.TrustedCardActionResponse, 8)
	var callers sync.WaitGroup
	for range cap(claims) {
		callers.Add(1)
		go func() {
			defer callers.Done()
			claims <- p.callback(core.TrustedCardAction{Kind: Kind, ApprovalID: text(planD["approval_id"]),
				Decision: core.ActionApprove, Language: core.LangEnglish,
				Principal: core.ActionPrincipal{UserID: "sender-1", ChatID: "group-1", SessionKey: key, MessageID: cardD}})
		}()
	}
	callers.Wait()
	close(claims)
	var approved core.TrustedCardActionResponse
	winners := 0
	for response := range claims {
		if response.Complete != nil {
			approved = response
			winners++
		}
	}
	if winners != 1 || approved.Card == nil || executes.Load() != 0 {
		t.Fatalf("concurrent claims must have exactly one not-yet-executed winner, got %d", winners)
	}
	completed := approved.Complete()
	if completed.Card == nil {
		t.Fatal("host did not return the actual execution receipt")
	}
	if err := p.RefreshCardMessage(context.Background(), cardD, key, completed.Card); err != nil {
		t.Fatal(err)
	}
	duplicate := click(planD, cardD, "sender-1", core.ActionApprove)
	if duplicate.Complete != nil || duplicate.Card != nil || duplicate.Toast != completed.Card.Header.Title {
		t.Fatal("duplicate callback must report the durable result without another card update")
	}
	p.mu.Lock()
	retained := p.cards[cardD]
	p.mu.Unlock()
	if retained == nil || retained.RenderText() != completed.Card.RenderText() {
		t.Fatal("duplicate callback replaced the published receipt")
	}
	verified := resultOf(planD)
	expectStatus(verified, "verified")
	receiptJSON, _ := json.Marshal(verified["receipt"])
	if !bytes.Contains(receiptJSON, []byte("Approved original fictional plan D")) {
		t.Fatalf("model cannot discuss the executed original receipt: %s", receiptJSON)
	}
	after := turn("Read current customer state and updated history", "customer", query)
	expectStatus(after, "found")
	customerJSON, _ := json.Marshal(after["customer"])
	if !bytes.Contains(customerJSON, []byte("Approved original fictional plan D")) || !bytes.Contains(customerJSON, []byte("Send fictional proposal")) {
		t.Fatalf("current customer summary did not match the actual write: %s", customerJSON)
	}
	historyJSON, _ := json.Marshal(after["history"])
	if len(after["history"].([]any)) != baselineHistory+1 || !bytes.Contains(historyJSON, []byte("Approved original fictional plan D")) || bytes.Contains(historyJSON, []byte("plan B")) || bytes.Contains(historyJSON, []byte("plan C")) {
		t.Fatalf("history does not contain exactly the approved follow-up: %s", historyJSON)
	}
	turn("Continue discussing the verified result and its next step; no new approval or write", "result", map[string]any{"change_id": planD["change_id"]})
	turn("Continue a general assistant discussion on the same session", "", nil)
	if executes.Load() != 1 || agent.starts.Load() != 1 || agent.closes.Load() != 0 || agent.permissions.Load() != 0 || agent.turns.Load() != int32(n) {
		t.Fatalf("host/session lifecycle: executions=%d starts=%d closes=%d permission interceptions=%d turns=%d/%d", executes.Load(), agent.starts.Load(), agent.closes.Load(), agent.permissions.Load(), agent.turns.Load(), n)
	}
	transcript := p.transcript()
	if strings.Contains(transcript, "INTERNAL_SPIKE_") || !strings.Contains(transcript, "Send fictional proposal") || !strings.Contains(transcript, text(planD["change_id"])) {
		t.Fatal("Quiet either leaked internals or hid business approval/receipt")
	}
	agent.mu.Lock()
	childEnv := strings.Join(agent.env, "\n")
	agent.mu.Unlock()
	if strings.Contains(childEnv, hostSecretEnv) || strings.Contains(childEnv, stageInputEnv) || strings.Contains(childEnv, a.hostSecret) {
		t.Fatal("child received privileged credentials or the old request-file handoff")
	}
	history := e.GetSessions().GetOrCreateActive(key).GetHistory(100)
	if len(history) < n*2 {
		t.Fatalf("normal conversation lost stage/results: %d history entries for %d turns", len(history), n)
	}
}
