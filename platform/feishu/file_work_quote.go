package feishu

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Feishu desktop sends file uploads separately even while composing a reply.
// Only an explicit @bot text reply may select that one same-sender upload.
// Do not walk reply chains or attach it to the sender's most recent work.
func (p *Platform) dispatchQuotedWorkFile(ctx context.Context, msgType, content string, mentions []*larkim.MentionEvent, msg *core.Message) {
	candidate := msg.FileWorkNewInput
	msg.FileWorkNewInput = false
	p.mu.RLock()
	enabled := p.fileWorkImages
	p.mu.RUnlock()
	var text struct {
		Text string `json:"text"`
	}
	if !candidate || !enabled || msg.FileWorkPrivate || !p.acceptFixedRoute(msg.UserID, msg.ChannelID) ||
		!fileRouteID(msg.ParentMessageID, 256) || !isBotMentioned(mentions, p.getBotOpenID()) {
		return
	}
	if msgType == "post" {
		var ok bool
		text.Text, _, ok = p.fileWorkPostText(content, false)
		if !ok {
			return
		}
	} else if msgType != "text" || json.Unmarshal([]byte(content), &text) != nil {
		return
	}
	text.Text = stripMentions(text.Text, mentions, p.getBotOpenID())
	if strings.TrimSpace(text.Text) == "" || p.isMessageRecalled(msg.ParentMessageID) {
		return
	}
	metadataCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	metadataCtx = context.WithValue(metadataCtx, fileDownloadLimitKey{}, int64(64<<10))
	metadataCtx = context.WithValue(metadataCtx, fileDeliveryRequestKey{}, true) // No metadata redirects.
	response, err := p.client.Im.Message.Get(metadataCtx, larkim.NewGetMessageReqBuilder().MessageId(msg.ParentMessageID).UserIdType("open_id").Build())
	if err != nil || response == nil || response.ApiResp == nil || len(response.RawBody) > 64<<10 {
		slog.Warn("feishu: selected file metadata unavailable")
		return
	}
	if !response.Success() || response.Data == nil || len(response.Data.Items) != 1 {
		return
	}
	item := response.Data.Items[0]
	if item == nil || item.Sender == nil || item.Body == nil ||
		stringValue(item.MessageId) != msg.ParentMessageID || stringValue(item.ChatId) != msg.ChannelID ||
		stringValue(item.MsgType) != "file" || (item.Deleted != nil && *item.Deleted) ||
		stringValue(item.Sender.SenderType) != "user" || stringValue(item.Sender.IdType) != "open_id" || stringValue(item.Sender.Id) != msg.UserID {
		return
	}
	var file struct {
		Key  string `json:"file_key"`
		Name string `json:"file_name"`
	}
	if json.Unmarshal([]byte(stringValue(item.Body.Content)), &file) != nil || file.Key == "" || file.Name == "" || p.isMessageRecalled(msg.ParentMessageID) {
		return
	}
	attachment := p.receiveFile(ctx, msg.ParentMessageID, file.Key, file.Name, &fileReceiveBudget{})
	if p.isMessageRecalled(msg.ParentMessageID) {
		return
	}
	msg.Files = []core.FileAttachment{attachment}
	msg.Content, msg.FileWorkNewInput = text.Text, true
}
