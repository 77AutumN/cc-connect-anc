package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var _ core.FileWorkReceiver = (*Platform)(nil)

// Called only by host startup after fixed file routing is enabled.
func (p *Platform) SetFileWorkImagesEnabled(enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if enabled && !p.fileWorkEnabled {
		return errors.New("file images require controlled file routing")
	}
	p.fileWorkImages = enabled
	return nil
}

func (p *Platform) receiveWorkImage(ctx context.Context, messageID, key string, budget *fileReceiveBudget) core.FileAttachment {
	f := p.receiveResource(ctx, messageID, key, "image", "image", budget)
	if f.ReceiveError != "" {
		return f
	}
	ext := ""
	switch f.MimeType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	}
	if ext == "" {
		f.Data = nil
		f.ReceiveError = core.MsgFileInputFormatUnsupported
		return f
	}
	f.FileName = fmt.Sprintf("image-%x%s", sha256.Sum256(f.Data), ext)
	return f
}

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
	p.mu.RLock()
	allowImages := p.fileWorkImages
	p.mu.RUnlock()
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
		text, keys, ok := p.fileWorkPostText(content, allowImages)
		if !ok {
			fail(core.MsgFileWorkUnavailable)
		} else {
			msg.Content = stripMentions(text, mentions, p.getBotOpenID())
			budget := &fileReceiveBudget{}
			for _, key := range keys {
				msg.Files = append(msg.Files, p.receiveWorkImage(ctx, msg.MessageID, key, budget))
			}
		}
	case "image":
		if !allowImages {
			fail(core.MsgFileWorkUnavailable)
			break
		}
		var body struct {
			Key string `json:"image_key"`
		}
		if json.Unmarshal([]byte(content), &body) != nil || body.Key == "" {
			fail(core.MsgFileInputUnavailable)
		} else {
			msg.Files = []core.FileAttachment{p.receiveWorkImage(ctx, msg.MessageID, body.Key, &fileReceiveBudget{})}
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
		// Audio and merged forwards require their own ownership rules.
		// Do not fall back to legacy media downloads or launch the old model path.
		fail(core.MsgFileWorkUnavailable)
	}
	p.dispatchCoreMessage(msg)
}

// fileWorkPostText accepts only complete textual posts. Validate every locale
// before selecting one, so an unsupported attachment cannot disappear silently.
func (p *Platform) fileWorkPostText(raw string, allowImages bool) (string, []string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || len(fields) == 0 {
		return "", nil, false
	}
	var posts []postLang
	if _, flat := fields["content"]; flat {
		var post postLang
		if json.Unmarshal([]byte(raw), &post) != nil {
			return "", nil, false
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
				return "", nil, false
			}
			posts = append(posts, post)
		}
	}
	var selectedKeys []string
	for i := range posts {
		var imageKeys []string
		if posts[i].Content == nil {
			return "", nil, false
		}
		for _, line := range posts[i].Content {
			for j := range line {
				elem := &line[j]
				switch elem.Tag {
				case "img":
					if !allowImages || elem.ImageKey == "" || len(imageKeys) >= maxFileInputCount {
						return "", nil, false
					}
					imageKeys = append(imageKeys, elem.ImageKey)
					// Extract text only after all resources/locale variants are checked.
					elem.Tag, elem.Text = "text", ""
				case "text", "a", "code_block", "markdown":
				case "at":
					if elem.UserName == "" {
						// Preserve the reference without requesting profile data.
						elem.UserName = elem.UserId
					}
				default:
					return "", nil, false
				}
			}
		}
		if i == 0 {
			selectedKeys = imageKeys
		} else if strings.Join(imageKeys, "\x00") != strings.Join(selectedKeys, "\x00") {
			return "", nil, false
		}
	}
	parts, _ := p.extractPostParts("", &posts[0])
	return strings.Join(parts, "\n"), selectedKeys, true
}
