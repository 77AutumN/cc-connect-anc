package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var _ core.FileWorkReceiver = (*Platform)(nil)

func (p *Platform) SetFileWorkReplyObserver(observer func(core.Message, string) error) {
	p.mu.Lock()
	p.fileReplyObserver = observer
	p.mu.Unlock()
}

func (p *Platform) FileWorkReplyContext(raw json.RawMessage) (any, error) {
	r, err := decodeFileReplyRoute(raw)
	if err != nil || !p.validFileReplyRoute(r) {
		return nil, core.ErrFileNotSubmitted
	}
	return replyContext{messageID: r.MessageID, chatID: r.ChatID, sessionKey: r.SessionKey,
		rootID: r.RootID, threadID: r.ThreadID, controlledFileWork: true}, nil
}

func (p *Platform) observeFileWorkReply(rc replyContext, receipt string) {
	p.mu.RLock()
	observer, enabled := p.fileReplyObserver, p.fileWorkEnabled
	p.mu.RUnlock()
	if !enabled || observer == nil || !rc.controlledFileWork || receipt == "" {
		return
	}
	if _, err := p.FileReplyRoute(rc); err != nil {
		return
	}
	msg := core.Message{Platform: p.Name(), UserID: p.allowFrom, ChannelID: rc.chatID,
		SessionKey: rc.sessionKey, MessageID: rc.messageID, ControlledFileWork: true}
	if err := observer(msg, receipt); err != nil {
		// The message is already sent. Returning an error here would invite a
		// duplicate send; leave the unrecorded reply unresolvable instead.
		slog.Error("file work: reply association failed; message will not be resent")
	}
}

// SetFileWorkEnabled is a host-only startup setting. Prepare still verifies
// uniqueness across the complete receiver group before any events are accepted.
func (p *Platform) SetFileWorkEnabled(enabled bool, authorizeReply func(core.Message, string) error) error {
	if enabled {
		if !p.strictRoutes || p.shareSessionInChannel || p.threadIsolation || p.shouldUseWebhookMode() || authorizeReply == nil {
			return errors.New("controlled file intake requires a fixed isolated receiver")
		}
		for _, value := range []string{p.allowFrom, p.allowChat} {
			if value == "" || strings.ContainsAny(value, "*, \t\r\n") {
				return errors.New("controlled file intake requires an exact sender and chat")
			}
		}
	}
	p.mu.Lock()
	p.fileWorkEnabled, p.fileReplyAuthorized = enabled, authorizeReply
	p.mu.Unlock()
	return nil
}

// Controlled intake never reads quoted/forwarded resources. The host resolves
// an explicit parent to an owned work before this path downloads current files.
func (p *Platform) dispatchFileWork(ctx context.Context, msgType, content string, mentions []*larkim.MentionEvent, msg *core.Message, parentAuthorized bool) {
	if msg.ParentMessageID != "" && !parentAuthorized {
		// Private or explicitly mentioned unknown replies reach the host only
		// for its location hint. Unmentioned group replies were already dropped.
		p.dispatchCoreMessage(msg)
		return
	}
	fail := func(key core.MsgKey) {
		msg.Content = ""
		msg.Files = []core.FileAttachment{{RequireSave: true, ReceiveError: key}}
	}
	switch msgType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &body) != nil {
			fail(core.MsgFileInputUnavailable)
		} else {
			msg.Content = stripMentions(body.Text, mentions, p.getBotOpenID())
		}
	case "post":
		text, ok := p.fileWorkPostText(content)
		if !ok {
			fail(core.MsgFileWorkUnavailable)
		} else {
			msg.Content = stripMentions(text, mentions, p.getBotOpenID())
		}
	case "file":
		var body struct {
			Key  string `json:"file_key"`
			Name string `json:"file_name"`
		}
		if json.Unmarshal([]byte(content), &body) != nil || body.Key == "" || body.Name == "" {
			fail(core.MsgFileInputUnavailable)
		} else {
			msg.Files = []core.FileAttachment{p.receiveFile(ctx, msg.MessageID, body.Key, body.Name, &fileReceiveBudget{})}
		}
	default:
		// Images, audio and merged forwards require their own ownership rules.
		// Do not fall back to legacy media downloads or launch the old model path.
		fail(core.MsgFileWorkUnavailable)
	}
	p.dispatchCoreMessage(msg)
}

// fileWorkPostText accepts only complete textual posts. Validate every locale
// before selecting one, so an unsupported attachment cannot disappear silently.
func (p *Platform) fileWorkPostText(raw string) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || len(fields) == 0 {
		return "", false
	}
	var posts []postLang
	if _, flat := fields["content"]; flat {
		var post postLang
		if json.Unmarshal([]byte(raw), &post) != nil {
			return "", false
		}
		posts = append(posts, post)
	} else {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var post postLang
			if json.Unmarshal(fields[key], &post) != nil {
				return "", false
			}
			posts = append(posts, post)
		}
	}
	for i := range posts {
		if posts[i].Content == nil {
			return "", false
		}
		for _, line := range posts[i].Content {
			for j := range line {
				elem := &line[j]
				switch elem.Tag {
				case "text", "a", "code_block", "markdown":
				case "at":
					if elem.UserName == "" {
						// Preserve the reference without requesting profile data.
						elem.UserName = elem.UserId
					}
				default:
					return "", false
				}
			}
		}
	}
	parts, _ := p.extractPostParts("", &posts[0])
	return strings.Join(parts, "\n"), true
}
