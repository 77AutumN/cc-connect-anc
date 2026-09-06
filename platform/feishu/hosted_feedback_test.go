package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type slowHostedRefresher struct{ hostedRefreshStub }

func (*slowHostedRefresher) RefreshCardMessage(ctx context.Context, _, _ string, _ *core.Card) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestHostedFeedbackOrderingAndRetryBudget(t *testing.T) {
	for _, failure := range []string{"none", "executing", "final"} {
		t.Run(failure, func(t *testing.T) {
			patches, executions, notices := 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "tenant_access_token"):
					writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fixture-token"})
				case r.Method == http.MethodPatch:
					patches++
					var body struct {
						Content string `json:"content"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					var card map[string]any
					if err := json.Unmarshal([]byte(body.Content), &card); err != nil {
						t.Error(err)
					}
					if card["config"].(map[string]any)["update_multi"] != true {
						t.Error("host card missing shared update")
					}
					if failure == "executing" || (failure == "final" && patches > 1) {
						writeJSON(t, w, map[string]any{"code": 230001, "msg": "fixture failure"})
					} else {
						writeJSON(t, w, map[string]any{"code": 0})
					}
				case strings.HasSuffix(r.URL.Path, "/fixture-card/reply"):
					notices++
					var body struct {
						UUID    string `json:"uuid"`
						Content string `json:"content"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.UUID != hostedNoticeUUID("fixture-card") || len(body.UUID) > 50 {
						t.Error("unstable notification UUID")
					}
					if !strings.Contains(body.Content, "verified fixture") {
						t.Error("notice lost actual outcome")
					}
					writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "notice"}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			p := &Platform{platformName: "feishu", client: lark.NewClient("feedback-"+failure, "fixture-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(srv.Client()))}
			p.self = &interactivePlatform{Platform: p}
			intermediate := &core.Card{SharedUpdate: true, Header: &core.CardHeader{Title: "executing"}}
			final := &core.Card{SharedUpdate: true, Header: &core.CardHeader{Title: "verified fixture"}}
			action := core.TrustedCardAction{Language: core.LangEnglish, Principal: core.ActionPrincipal{MessageID: "fixture-card", ChatID: "fixture-chat", SessionKey: "fixture-session"}}
			p.startHostedActionCompletion(core.TrustedCardActionResponse{Card: intermediate, Complete: func() core.TrustedCardActionResponse {
				executions++
				return core.TrustedCardActionResponse{Card: final}
			}}, action)
			wantPatches, wantNotices := 2, 0
			if failure == "executing" {
				wantPatches, wantNotices = 1, 1
			}
			if failure == "final" {
				wantPatches, wantNotices = 4, 1
			}
			if patches != wantPatches || notices != wantNotices || executions != 1 {
				t.Fatalf("patches=%d notices=%d business=%d", patches, notices, executions)
			}
		})
	}
}

type lostNoticeResponse struct {
	base http.RoundTripper
	lost bool
}

func (rt *lostNoticeResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if strings.HasSuffix(req.URL.Path, "/reply") && err == nil {
		if !rt.lost {
			rt.lost = true
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
	}
	return resp, err
}

func TestHostedNoticeLostResponseReusesUUID(t *testing.T) {
	ids := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "fixture-token"})
			return
		}
		var body struct {
			UUID string `json:"uuid"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		ids[body.UUID]++
		writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "same-notice"}})
	}))
	defer srv.Close()
	httpClient := srv.Client()
	httpClient.Transport = &lostNoticeResponse{base: httpClient.Transport}
	p := &Platform{platformName: "feishu", client: lark.NewClient("lost-notice", "fixture-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(httpClient))}
	p.notifyHostedDisplayFailure(core.TrustedCardAction{Language: core.LangEnglish, Principal: core.ActionPrincipal{MessageID: "fixture"}}, core.NewCard().Title("verified", "green").Build())
	if len(ids) != 1 || ids[hostedNoticeUUID("fixture")] < 2 {
		t.Fatalf("delivery retry did not reuse a single UUID: %v", ids)
	}
}

func TestHostedSharedUpdateIsOptIn(t *testing.T) {
	ordinary := core.NewCard().Title("navigation", "blue").Build()
	if _, present := renderCardMap(ordinary, "session")["config"].(map[string]any)["update_multi"]; present {
		t.Fatal("ordinary cards changed")
	}
	ordinary.SharedUpdate = true
	if renderCardMap(ordinary, "session")["config"].(map[string]any)["update_multi"] != true {
		t.Fatal("shared update missing")
	}
}

func TestHostedUnknownIntermediateNeverSendsFinalPatch(t *testing.T) {
	stub := &hostedRefreshStub{err: errors.New("response lost after server accepted PATCH")}
	p := &Platform{self: stub}
	executions := 0
	p.startHostedActionCompletion(core.TrustedCardActionResponse{Card: core.NewCard().Title("executing", "blue").Build(), Complete: func() core.TrustedCardActionResponse {
		executions++
		return core.TrustedCardActionResponse{Card: core.NewCard().Title("verified", "green").Build()}
	}}, core.TrustedCardAction{Principal: core.ActionPrincipal{MessageID: "fixture"}})
	if len(stub.snapshot()) != 1 || executions != 1 {
		t.Fatal("uncertain intermediate caused competing final update or business retry")
	}
}

func TestHostedApprovalSlowPatchCannotConsumeCallbackBudget(t *testing.T) {
	p := &Platform{platformName: "feishu", self: &slowHostedRefresher{}}
	finished := make(chan struct{})
	p.trustedCardActionHandler = func(core.TrustedCardAction) core.TrustedCardActionResponse {
		time.Sleep(1500 * time.Millisecond)
		return core.TrustedCardActionResponse{
			Card:     core.NewCard().Title("executing", "blue").Build(),
			Complete: func() core.TrustedCardActionResponse { close(finished); return core.TrustedCardActionResponse{} },
		}
	}
	start := time.Now()
	response, handled := p.handleHostedCardAction(map[string]any{"kind": "fixture", "approval_id": "apr_fixture", "decision": "approve"}, "owner", "chat", "card", "session")
	elapsed := time.Since(start)
	if !handled || response == nil || response.Toast == nil {
		t.Fatal("missing immediate acknowledgement")
	}
	if elapsed >= 3*time.Second {
		t.Errorf("callback blocked on card network I/O for %s (platform limit: 3s)", elapsed)
	}
	select {
	case <-finished:
	case <-time.After(4 * time.Second):
		t.Fatal("accepted decision was not executed")
	}
}
