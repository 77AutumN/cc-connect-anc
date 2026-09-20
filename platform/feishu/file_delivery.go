package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var _ core.FileReceiptSender = (*Platform)(nil)

var errFileAcceptanceUnknown = errors.New("file delivery acceptance unconfirmed")

type fileDeliveryRequestKey struct{}

// fileReplyRoute is host-only state. It must never be accepted from a model
// tool's input or reconstructed from the latest interactive reply context.
type fileReplyRoute struct {
	Version       int    `json:"version"`
	Platform      string `json:"platform"`
	AppID         string `json:"app_id"`
	SenderID      string `json:"sender_id"`
	ChatID        string `json:"chat_id"`
	SessionKey    string `json:"session_key"`
	MessageID     string `json:"message_id"`
	RootID        string `json:"root_id"`
	ThreadID      string `json:"thread_id"`
	Reply         bool   `json:"reply"`
	ReplyInThread bool   `json:"reply_in_thread"`
}

func (p *Platform) FileReplyRoute(replyCtx any) (json.RawMessage, error) {
	rc, ok := replyCtx.(replyContext)
	if !ok {
		return nil, core.ErrFileNotSubmitted
	}
	inThread := rc.rootID != "" || rc.threadID != ""
	route := fileReplyRoute{
		Version: 1, Platform: p.Name(), AppID: p.appID, SenderID: p.allowFrom,
		ChatID: rc.chatID, SessionKey: rc.sessionKey, MessageID: rc.messageID,
		RootID: rc.rootID, ThreadID: rc.threadID,
		Reply: !p.noReplyToTrigger || inThread, ReplyInThread: inThread,
	}
	if !p.validFileReplyRoute(route) {
		return nil, core.ErrFileNotSubmitted
	}
	return json.Marshal(route)
}

func (p *Platform) validFileReplyRoute(r fileReplyRoute) bool {
	if !p.strictRoutes || p.shareSessionInChannel || p.threadIsolation ||
		r.Version != 1 || r.Platform != p.Name() || r.AppID == "" || r.AppID != p.appID ||
		r.SenderID != p.allowFrom || r.ChatID != p.allowChat ||
		!p.acceptFixedRoute(r.SenderID, r.ChatID) {
		return false
	}
	for _, value := range []string{p.allowFrom, p.allowChat} {
		if value == "" || strings.ContainsAny(value, "*, \t\r\n") {
			return false
		}
	}
	if r.SessionKey != p.Name()+":"+r.ChatID+":"+r.SenderID || !fileRouteID(r.MessageID, 256) {
		return false
	}
	for _, value := range []string{r.RootID, r.ThreadID} {
		if value != "" && !fileRouteID(value, 256) {
			return false
		}
	}
	inThread := r.RootID != "" || r.ThreadID != ""
	return r.ReplyInThread == inThread && (!inThread || r.Reply)
}

func fileRouteID(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func decodeFileReplyRoute(raw json.RawMessage) (fileReplyRoute, error) {
	var route fileReplyRoute
	if len(raw) == 0 || len(raw) > 4096 {
		return route, core.ErrFileNotSubmitted
	}
	fields := map[string]bool{"version": false, "platform": false, "app_id": false, "sender_id": false, "chat_id": false, "session_key": false, "message_id": false, "root_id": false, "thread_id": false, "reply": false, "reply_in_thread": false}
	d := json.NewDecoder(bytes.NewReader(raw))
	if start, err := d.Token(); err != nil || start != json.Delim('{') {
		return route, core.ErrFileNotSubmitted
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return route, core.ErrFileNotSubmitted
		}
		name, ok := key.(string)
		seen, known := fields[name]
		if !ok || !known || seen {
			return route, core.ErrFileNotSubmitted
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return route, core.ErrFileNotSubmitted
		}
		fields[name] = true
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return route, core.ErrFileNotSubmitted
	}
	var extra json.RawMessage
	if d.Decode(&extra) != io.EOF {
		return route, core.ErrFileNotSubmitted
	}
	for _, present := range fields {
		if !present {
			return route, core.ErrFileNotSubmitted
		}
	}
	if json.Unmarshal(raw, &route) != nil {
		return fileReplyRoute{}, core.ErrFileNotSubmitted
	}
	return route, nil
}

// SendFileWithReceipt submits once to the frozen destination. An error without
// ErrFileNotSubmitted means acceptance is unknown, including a lost response;
// the host must retain that state instead of automatically resending.
func (p *Platform) SendFileWithReceipt(ctx context.Context, raw json.RawMessage, file core.FileAttachment, deliveryID string) (string, error) {
	route, err := decodeFileReplyRoute(raw)
	if err != nil || !p.validFileReplyRoute(route) || !fileRouteID(deliveryID, 50) ||
		len(file.Data) == 0 || len(file.Data) > core.DefaultFileInputLimit || file.FileName == "" ||
		file.ReceiveError != "" || ctx.Err() != nil {
		return "", core.ErrFileNotSubmitted
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, fileDeliveryRequestKey{}, true)
	msgType, content, err := p.uploadFile(ctx, file, false)
	if err != nil || ctx.Err() != nil {
		return "", core.ErrFileNotSubmitted
	}
	if route.Reply {
		body := larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).Content(content).
			Uuid(deliveryID).ReplyInThread(route.ReplyInThread).Build()
		resp, err := p.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().MessageId(route.MessageID).Body(body).Build())
		if err != nil || resp == nil || resp.ApiResp == nil || resp.StatusCode >= 500 {
			return "", errFileAcceptanceUnknown
		}
		if !resp.Success() {
			return "", core.ErrFileNotSubmitted
		}
		if resp.StatusCode != http.StatusOK || resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
			return "", errFileAcceptanceUnknown
		}
		return *resp.Data.MessageId, nil
	}
	resp, err := p.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(route.ChatID).MsgType(msgType).Content(content).Uuid(deliveryID).Build()).Build())
	if err != nil || resp == nil || resp.ApiResp == nil || resp.StatusCode >= 500 {
		return "", errFileAcceptanceUnknown
	}
	if !resp.Success() {
		return "", core.ErrFileNotSubmitted
	}
	if resp.StatusCode != http.StatusOK || resp.Data == nil || resp.Data.MessageId == nil || *resp.Data.MessageId == "" {
		return "", errFileAcceptanceUnknown
	}
	return *resp.Data.MessageId, nil
}
