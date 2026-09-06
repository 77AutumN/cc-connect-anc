package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func (p *Platform) refreshHostedFeedback(action core.TrustedCardAction, card *core.Card, phase string) bool {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), hostedActionTimeout)
	defer cancel()
	err := p.refreshHostedActionCard(ctx, action.Principal.MessageID, action.Principal.SessionKey, card)
	classification := "confirmed"
	if err != nil {
		classification = "unconfirmed"
	}
	// No customer content, callback identities, tokens, or raw API errors.
	slog.Info("hosted card feedback", "phase", phase, "elapsed_ms", time.Since(start).Milliseconds(), "result", classification)
	return err == nil
}

func (p *Platform) refreshHostedFinal(action core.TrustedCardAction, card *core.Card) bool {
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
		}
		// All attempts send the same immutable terminal card. Never execute here.
		if p.refreshHostedFeedback(action, card, "final") {
			return true
		}
	}
	return false
}

func hostedNoticeUUID(messageID string) string {
	sum := sha256.Sum256([]byte("hosted-display-failure\x00" + messageID))
	return "hdf-" + hex.EncodeToString(sum[:20])
}

func (p *Platform) notifyHostedDisplayFailure(action core.TrustedCardAction, final *core.Card) {
	i18n := core.NewI18n(action.Language)
	outcome := i18n.T(core.MsgHostedActionResultUnknownToast)
	if final != nil && final.Header != nil {
		outcome = final.Header.Title
	}
	content, _ := json.Marshal(map[string]string{"text": i18n.Tf(core.MsgHostedActionDisplayFailedFmt, outcome)})
	if p.client == nil || action.Principal.MessageID == "" {
		slog.Warn("hosted display failure notice unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Quote the exact original approval, preserving the existing thread policy.
	rc := replyContext{messageID: action.Principal.MessageID, chatID: action.Principal.ChatID, sessionKey: action.Principal.SessionKey}
	_, err := p.replyMessageResultWithUUID(ctx, rc, larkim.MsgTypeText, string(content), true, hostedNoticeUUID(action.Principal.MessageID))
	if err != nil {
		// Retry only message delivery, using the same server-side UUID above.
		// No restart recovery or business replay is implied by this notification.
		slog.Warn("hosted display failure notice unconfirmed")
	}
}
