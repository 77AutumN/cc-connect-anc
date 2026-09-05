package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type actionToolHostStub struct {
	actionHostStub
	toolCalls     int
	toolPrincipal ActionPrincipal
	toolToken     string
	toolCommand   string
	toolDeadline  bool
	toolErr       error
	toolResult    map[string]any
	toolCard      *ActionHostResult
}

func (h *actionToolHostStub) Tool(ctx context.Context, command string, input json.RawMessage, p ActionPrincipal, token string, _ Language) (map[string]any, *ActionHostResult, error) {
	h.toolCalls++
	h.toolPrincipal, h.toolToken, h.toolCommand = p, token, command
	_, h.toolDeadline = ctx.Deadline()
	return h.toolResult, h.toolCard, h.toolErr
}

func actionToolFixture(t *testing.T) (*Engine, *actionToolHostStub, *hostedCardPlatform, *interactiveState) {
	t.Helper()
	p := &hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	h := &actionToolHostStub{toolResult: map[string]any{"status": "ok", "customer": "fixture"}}
	e := NewEngine("test-project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetActionHost(h)
	principal := ActionPrincipal{Platform: "test", UserID: "owner", ChatID: "chat", SessionKey: "test:chat:owner", Project: "test-project", MessageID: "request"}
	state := &interactiveState{agentSession: newCUJAgentSession(), platform: p, replyCtx: "reply", actionToken: "test-session-token", currentPrincipal: principal, actionPrincipal: principal}
	e.interactiveStates[principal.SessionKey] = state
	return e, h, p, state
}

func actionToolRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestActionToolHandlerScopedReadsAndStrictBoundary(t *testing.T) {
	e, h, _, _ := actionToolFixture(t)
	handler := e.ActionToolHandler()
	for _, command := range []string{"customer", "result"} {
		w := actionToolRequest(handler, "POST", "/tool", "test-session-token", `{"command":"`+command+`","input":{}}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"customer":"fixture"`) {
			t.Fatalf("valid %s = %d %s", command, w.Code, w.Body.String())
		}
	}
	if h.toolPrincipal.UserID != "owner" || h.toolPrincipal.Project != "test-project" || h.toolToken != "test-session-token" || !h.toolDeadline {
		t.Fatal("tool lost host principal, token or deadline")
	}
	for _, tc := range []struct{ name, method, path, token, body string }{
		{"missing token", "POST", "/tool", "", `{"command":"customer","input":{}}`},
		{"wrong token", "POST", "/tool", "other", `{"command":"customer","input":{}}`},
		{"method", "GET", "/tool", "test-session-token", `{"command":"customer","input":{}}`},
		{"privileged route", "POST", "/cron/add", "test-session-token", `{"command":"customer","input":{}}`},
		{"query identity", "POST", "/tool?actor=other", "test-session-token", `{"command":"customer","input":{}}`},
		{"actor envelope", "POST", "/tool", "test-session-token", `{"command":"customer","input":{},"actor":"other"}`},
		{"missing input", "POST", "/tool", "test-session-token", `{"command":"customer"}`},
		{"null input", "POST", "/tool", "test-session-token", `{"command":"customer","input":null}`},
		{"array input", "POST", "/tool", "test-session-token", `{"command":"customer","input":[]}`},
		{"trailing json", "POST", "/tool", "test-session-token", `{"command":"customer","input":{}} {}`},
		{"duplicate command", "POST", "/tool", "test-session-token", `{"command":"customer","command":"stage","input":{}}`},
		{"apply", "POST", "/tool", "test-session-token", `{"command":"apply","input":{}}`},
		{"approve", "POST", "/tool", "test-session-token", `{"command":"approve","input":{}}`},
		{"raw api", "POST", "/tool", "test-session-token", `{"command":"raw-api","input":{}}`},
		{"shell", "POST", "/tool", "test-session-token", `{"command":"shell","input":{}}`},
		{"oversize", "POST", "/tool", "test-session-token", `{"command":"customer","input":{"content":"` + strings.Repeat("a", 65536) + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.toolCalls
			w := actionToolRequest(handler, tc.method, tc.path, tc.token, tc.body)
			if w.Code < 400 || h.toolCalls != before {
				t.Fatalf("boundary accepted: %d %s calls=%d", w.Code, w.Body.String(), h.toolCalls-before)
			}
		})
	}
}

func TestActionToolHandlerRejectsStoppedReboundAndRetiredSessions(t *testing.T) {
	for _, variant := range []string{"stopped", "closed", "retired", "sender", "chat", "session", "project", "platform", "engine-stopped"} {
		t.Run(variant, func(t *testing.T) {
			e, h, _, state := actionToolFixture(t)
			switch variant {
			case "stopped":
				state.markStopped()
			case "closed":
				_ = state.agentSession.Close()
			case "retired":
				delete(e.interactiveStates, state.currentPrincipal.SessionKey)
			case "sender":
				state.currentPrincipal.UserID = "other"
			case "chat":
				state.currentPrincipal.ChatID = "other"
			case "session":
				state.currentPrincipal.SessionKey = "other"
			case "project":
				state.currentPrincipal.Project = "other"
			case "platform":
				state.currentPrincipal.Platform = "other"
			case "engine-stopped":
				e.cancel()
			}
			w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", "test-session-token", `{"command":"customer","input":{}}`)
			if w.Code != 401 || h.toolCalls != 0 {
				t.Fatalf("retired/rebound token accepted: %d %s", w.Code, w.Body.String())
			}
		})
	}
	e, h, _, state := actionToolFixture(t)
	state.currentPrincipal.MessageID = "later-message-same-sender"
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", "test-session-token", `{"command":"result","input":{}}`)
	if w.Code != 200 || h.toolPrincipal.MessageID != "later-message-same-sender" {
		t.Fatalf("normal continuation refused: %d %s", w.Code, w.Body.String())
	}
}

func TestActionToolHandlerReportsCardDeliveryBeforeReturningStage(t *testing.T) {
	for _, failure := range []string{"", "publish", "bind", "tool"} {
		t.Run(failure, func(t *testing.T) {
			e, h, p, _ := actionToolFixture(t)
			h.toolResult = map[string]any{"status": "staged", "change_id": "chg_fixture"}
			h.toolCard = &ActionHostResult{Kind: h.Kind(), Status: "pending", ApprovalID: "apr_fixture", ChangeID: "chg_fixture", Card: NewCard().Markdown("Canonical fixture preview").Build()}
			if failure == "publish" {
				p.publishErr = errors.New("secret test failure must not leak")
			}
			if failure == "bind" {
				h.bindErr = errors.New("secret test failure must not leak")
			}
			if failure == "tool" {
				h.toolErr = errors.New("secret test failure must not leak")
			}
			w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", "test-session-token", `{"command":"stage","input":{}}`)
			if failure == "" {
				if w.Code != 200 || len(h.cardBindings) != 1 || len(p.refreshes) != 1 || !strings.Contains(w.Body.String(), `"status":"staged"`) {
					t.Fatalf("not published before result: %d %s", w.Code, w.Body.String())
				}
			} else if !strings.Contains(w.Body.String(), `"status":"unavailable"`) || !strings.Contains(w.Body.String(), `"change_id":"chg_fixture"`) {
				t.Fatalf("delivery failure lost persisted change: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "secret test failure") {
				t.Fatal("raw backend error leaked")
			}
		})
	}
}

func TestActionToolHandlerContextCancellationAndNoAdapter(t *testing.T) {
	e, h, _, _ := actionToolFixture(t)
	e.SetActionHost(&actionHostStub{})
	w := actionToolRequest(e.ActionToolHandler(), "POST", "/tool", "test-session-token", `{"command":"customer","input":{}}`)
	if w.Code != 503 {
		t.Fatalf("unsupported adapter: %d", w.Code)
	}
	e, h, _, _ = actionToolFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	r := httptest.NewRequest("POST", "/tool", strings.NewReader(`{"command":"customer","input":{}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer test-session-token")
	w = httptest.NewRecorder()
	e.ActionToolHandler().ServeHTTP(w, r)
	if w.Code != 503 || h.toolCalls != 0 {
		t.Fatalf("cancelled request called host: %d calls=%d", w.Code, h.toolCalls)
	}
}

type cancelledPublishPlatform struct {
	hostedCardPlatform
	cancel    context.CancelFunc
	fallbacks int
}

func (p *cancelledPublishPlatform) ReplyHostedActionPlaceholder(ctx context.Context, _ any, _ *Card) (string, error) {
	p.cancel()
	return "", ctx.Err()
}

func (p *cancelledPublishPlatform) ReplyCard(_ context.Context, _ any, _ *Card) error {
	p.fallbacks++
	return nil
}

func TestActionToolHandlerCancelledPublishDoesNotSendBackgroundFallback(t *testing.T) {
	e, h, _, state := actionToolFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &cancelledPublishPlatform{hostedCardPlatform: hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}, cancel: cancel}
	state.platform = p
	h.toolResult = map[string]any{"status": "pending", "change_id": "chg_fixture"}
	h.toolCard = &ActionHostResult{Kind: h.Kind(), Status: "pending", ApprovalID: "apr_fixture", ChangeID: "chg_fixture", Card: NewCard().Markdown("Fixture approval").Build()}
	r := httptest.NewRequest("POST", "/tool", strings.NewReader(`{"command":"stage","input":{}}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer test-session-token")
	w := httptest.NewRecorder()
	e.ActionToolHandler().ServeHTTP(w, r)
	if w.Code != 503 || p.fallbacks != 0 || len(h.cardBindings) != 0 {
		t.Fatalf("cancelled publication escaped request context: status=%d fallback=%d bindings=%d", w.Code, p.fallbacks, len(h.cardBindings))
	}
}

type actionToolJourneyAgent struct{ gatewaySpikeAgent }

func (*actionToolJourneyAgent) SetSessionEnv([]string) {}

func TestCUJ_ACTIONTOOL1_ReadStageResultContinueWithPinnedSender(t *testing.T) {
	p := &gatewaySpikePlatform{hostedCardPlatform: hostedCardPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}
	h := &actionToolHostStub{toolResult: map[string]any{"status": "ok", "customer": "fixture"}}
	a := &actionToolJourneyAgent{}
	e := NewEngine("test-project", a, []Platform{p}, "", LangEnglish)
	e.SetActionHost(h)
	handler := e.ActionToolHandler()
	a.tool = func(prompt string) string {
		h.mu.Lock()
		token := h.envTokens[len(h.envTokens)-1]
		h.mu.Unlock()
		command := "customer"
		if strings.Contains(prompt, "stage") {
			command = "stage"
			h.toolCard = &ActionHostResult{Kind: h.Kind(), Status: "pending", ApprovalID: "apr_journey", ChangeID: "chg_journey", Card: NewCard().Markdown("Canonical approval shown without process stop").Build()}
		} else {
			h.toolCard = nil
		}
		if strings.Contains(prompt, "result") {
			command = "result"
		}
		w := actionToolRequest(handler, "POST", "/tool", token, `{"command":"`+command+`","input":{}}`)
		return w.Body.String()
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine, agent: &a.cujAgent}
	for i, tc := range []struct{ user, content, want string }{
		{"owner", "customer history", `"customer":"fixture"`},
		{"owner", "stage original plan", `"customer":"fixture"`},
		{"owner", "result and discussion", `"customer":"fixture"`},
		{"intruder", "customer history in accidentally shared session", `"code":"invalid_session"`},
		{"owner", "customer history again", `"customer":"fixture"`},
	} {
		p.clearSent()
		e.ReceiveMessage(p, &Message{SessionKey: "test:chat:shared", Platform: "test", UserID: tc.user, ChannelID: "chat", MessageID: fmt.Sprintf("request-%d", i), Content: tc.content, ReplyCtx: "reply"})
		env.waitFor(tc.content, 2*time.Second, func() bool { return env.sentContains(tc.want) })
		if i == 1 && !env.sentContains("Canonical approval shown") {
			t.Fatal("host card missing from stage journey")
		}
	}
	a.mu.Lock()
	starts := len(a.sessions)
	a.mu.Unlock()
	if starts != 1 || h.toolCalls != 4 {
		t.Fatalf("unexpected restarts or unauthorized host call: starts=%d calls=%d", starts, h.toolCalls)
	}
}
