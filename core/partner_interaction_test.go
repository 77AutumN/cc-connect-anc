package core

import (
	"strings"
	"testing"
	"time"
)

func TestOldInteractionCannotAnswerReusedNativeRequest(t *testing.T) {
	e, _, p, state := actionToolFixture(t)
	pending := &pendingPermission{RequestID: "native-1", Interaction: CardInteraction{
		RequestID: "fresh-host-id", Principal: state.currentPrincipal,
	}, Questions: []UserQuestion{{Question: "first"}, {Question: "second"}}, CurrentQuestion: 1}
	state.pending = pending
	for _, id := range []string{"old-host-id:1", "fresh-host-id:0"} {
		msg := &Message{SessionKey: state.currentPrincipal.SessionKey, UserID: "owner", Content: "askq:0:1", InteractionRequestID: id}
		if !e.handlePendingPermission(p, msg, msg.Content, msg.SessionKey) {
			t.Fatal("stale callback reached model")
		}
		if pending.Answers != nil {
			t.Fatal("old card answered new question")
		}
	}
}

func TestCUJ_PARTNER1_RestrictedHelpNewAndStopSurviveBusinessThrottle(t *testing.T) {
	env := newCUJEnv(t)
	env.engine.SetConversationOnly()
	env.userSends("partner", "/history")
	env.waitFor("history refused", time.Second, func() bool { return env.sentContains("disabled") })
	env.plat.clearSent()
	env.userSends("partner", "/help")
	if !env.sentContains("/new") || !env.sentContains("/stop") {
		t.Fatal("necessary help missing")
	}
	env.plat.clearSent()
	env.userSends("partner", "/new isolated")
	if len(env.plat.getSent()) == 0 {
		t.Fatal("new session feedback missing")
	}
	env.engine.SetRateLimitCfg(RateLimitCfg{Window: time.Minute, MaxMessages: 1})
	if !env.engine.checkRateLimit(&Message{SessionKey: "test:partner", UserID: "partner"}) {
		t.Fatal("first rate-limit reservation denied")
	}
	t.Cleanup(env.engine.rateLimiter.Stop)
	env.userSends("partner", "blocked burst")
	env.plat.clearSent()
	env.userSends("partner", "/stop")
	if strings.Contains(strings.Join(env.plat.getSent(), "\n"), env.engine.i18n.T(MsgRateLimited)) {
		t.Fatal("stop was throttled")
	}
}
