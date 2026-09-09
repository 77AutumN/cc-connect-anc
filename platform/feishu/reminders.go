package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// SendReminder only uses this platform's fixed destination. It never starts
// another receiver, invokes an agent, or falls back to a different chat.
func (p *Platform) SendReminder(ctx context.Context, chatID, text, id string) (string, error) {
	if !p.strictRoutes || chatID == "" || chatID != p.allowChat || id == "" || strings.ContainsAny(p.allowChat, "*, \t\n") {
		return "", core.ErrReminderPermission
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", errors.New("invalid reminder text")
	}
	req := larkim.NewCreateMessageReqBuilder().ReceiveIdType(larkim.ReceiveIdTypeChatId).Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("text").Content(string(content)).Uuid(id).Build()).Build()
	resp, err := p.client.Im.Message.Create(ctx, req)
	if err != nil {
		return "", errors.New("reminder acceptance unconfirmed")
	}
	if !resp.Success() {
		// Permission failures are terminal. Do not log API messages, which can
		// contain destination details. Other errors use the durable backoff.
		message := strings.ToLower(resp.Msg)
		if resp.StatusCode == 403 || strings.Contains(message, "permission") || strings.Contains(message, "stopped receiving") || strings.Contains(message, "forbidden") {
			return "", core.ErrReminderPermission
		}
		switch resp.Code {
		case 99991672, 99991679, 230002, 230006, 230013, 230027:
			return "", core.ErrReminderPermission
		}
		return "", errors.New("reminder acceptance unconfirmed")
	}
	if resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
		return "", errors.New("reminder receipt missing")
	}
	return *resp.Data.MessageId, nil
}
