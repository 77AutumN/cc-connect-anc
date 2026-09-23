package feishu

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestFileWorkImagesUseCurrentResourceAndSharedFileBudget(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	p, got := fileWorkFixture(t, func(_ core.Message, parent string) bool { return parent == "known" }, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("type") != "image" || !strings.Contains(r.URL.Path, "current-") {
			t.Error("wrong resource owner/type", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(picture.Bytes())
	})
	if err := p.SetFileWorkImagesEnabled(true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, kind, body string
		count          int
	}{
		{"current-image", "image", `{"image_key":"fictional"}`, 1},
		{"current-post", "post", `{"content":[[{"tag":"text","text":"Use this fictional material"},{"tag":"img","image_key":"fictional"}]]}`, 1},
	} {
		msg := receiveFileWorkMessage(t, p, got, fileWorkEvent(tc.id, tc.kind, tc.body, "group", "known", false))
		if len(msg.Files) != tc.count || !bytes.Equal(msg.Files[0].Data, picture.Bytes()) || !strings.HasSuffix(msg.Files[0].FileName, ".png") || !msg.Files[0].RequireSave {
			t.Fatal("image lost current work attachment")
		}
	}
	before := requests.Load()
	msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("unknown-image", "image", `{"image_key":"not-selected"}`, "p2p", "unknown", false))
	if len(msg.Files) != 0 || requests.Load() != before {
		t.Fatal("unknown reply fetched image")
	}
	for _, body := range []string{
		`{"content":[[{"tag":"img","image_key":"a"},{"tag":"img","image_key":"b"},{"tag":"img","image_key":"c"},{"tag":"img","image_key":"d"},{"tag":"img","image_key":"e"}]]}`,
		`{"en_us":{"content":[[{"tag":"text","text":"hello"}]]},"zh_cn":{"content":[[{"tag":"img","image_key":"hidden"}]]}}`,
	} {
		if _, _, ok := p.fileWorkPostText(body, true); ok {
			t.Fatal("oversize or inconsistent locale accepted")
		}
	}
}

func fileWorkFixture(t *testing.T, authorize func(core.Message, string) bool, resource func(http.ResponseWriter, *http.Request)) (*Platform, <-chan *core.Message) {
	t.Helper()
	p := imageReceiverFixture(t, resource)
	p.appID, p.strictRoutes, p.allowFrom, p.allowChat = t.Name(), true, "sender-fixture", "chat-fixture"
	p.botOpenID, p.dedup = "bot-fixture", &core.MessageDedup{}
	p.sharedGroup = &sharedWSGroup{platforms: []*Platform{p}}
	got := make(chan *core.Message, 8)
	p.handler = func(_ core.Platform, msg *core.Message) { got <- msg }
	if err := p.SetFileWorkEnabled(true, func(m core.Message, parent string) error {
		if authorize(m, parent) {
			return nil
		}
		return core.ErrFileWorkNotFound
	}); err != nil {
		t.Fatal(err)
	}
	return p, got
}

func fileWorkEvent(id, kind, content, chatType, parent string, mentioned bool) *larkim.P2MessageReceiveV1 {
	msg := &larkim.EventMessage{
		MessageId: strPtr(id), ChatId: strPtr("chat-fixture"), ChatType: strPtr(chatType),
		MessageType: strPtr(kind), Content: strPtr(content), ParentId: strPtr(parent),
	}
	if mentioned {
		msg.Mentions = []*larkim.MentionEvent{{Key: strPtr("@_bot"), Id: &larkim.UserId{OpenId: strPtr("bot-fixture")}}}
	}
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender:  &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strPtr("sender-fixture")}, SenderType: strPtr("user")},
		Message: msg,
	}}
}

func receiveFileWorkMessage(t *testing.T, p *Platform, got <-chan *core.Message, event *larkim.P2MessageReceiveV1) *core.Message {
	t.Helper()
	if err := p.onMessage(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		if !msg.ControlledFileWork || msg.Platform != "feishu" || msg.UserID != "sender-fixture" ||
			msg.FileWorkPrivate != (*event.Event.Message.ChatType == "p2p") ||
			msg.ChannelID != "chat-fixture" || msg.SessionKey != "feishu:chat-fixture:sender-fixture" ||
			msg.MessageID != *event.Event.Message.MessageId || msg.ParentMessageID != *event.Event.Message.ParentId ||
			msg.ExtraContent != "" || len(msg.Images) != 0 || msg.Audio != nil {
			t.Fatal("controlled identity or parent was lost, or legacy content leaked")
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("controlled event was not dispatched")
		return nil
	}
}

func TestFileWorkTextReceiptsUseTriggerEvenWhenAssociationFails(t *testing.T) {
	for _, mode := range []string{"reply", "create", "preview"} {
		t.Run(mode, func(t *testing.T) {
			p, observed := fileDeliveryFixture(t, "success")
			if err := p.SetFileWorkEnabled(true, func(core.Message, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			calls := 0
			p.SetFileWorkReplyObserver(func(msg core.Message, receipt string) error {
				calls++
				if msg.MessageID != "origin-message" || msg.UserID != "sender-fixture" || receipt != "accepted-file" {
					t.Error("text reply was associated with another trigger")
				}
				return errors.New("simulated storage failure")
			})
			rc := fileDeliverySource()
			rc.controlledFileWork = true
			p.noReplyToTrigger = mode == "create"
			var err error
			if mode == "preview" {
				p.useInteractiveCard = true
				_, err = p.SendPreviewStart(context.Background(), rc, "fictional result")
			} else {
				err = p.Reply(context.Background(), rc, "fictional result")
			}
			if err != nil || calls != 1 || len(observed.paths) != 1 {
				t.Fatalf("association failure retried a sent message: %v, callbacks=%d, requests=%d", err, calls, len(observed.paths))
			}
		})
	}
}

func TestFileWorkKnownParentAllowsUnmentionedTextAndBoundedFile(t *testing.T) {
	var authorizationCalls, requests atomic.Int32
	p, got := fileWorkFixture(t, func(msg core.Message, parent string) bool {
		authorizationCalls.Add(1)
		if msg.Platform != "feishu" || msg.UserID != "sender-fixture" || msg.ChannelID != "chat-fixture" ||
			msg.SessionKey != "feishu:chat-fixture:sender-fixture" || msg.MessageID == "" ||
			msg.ParentMessageID != parent || !msg.ControlledFileWork {
			t.Error("authorizer received incomplete transport identity")
		}
		return parent == "known-parent"
	}, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.Contains(r.URL.Path, "/resources/") || r.URL.Query().Get("type") != "file" {
			t.Error("controlled intake fetched quoted content or profile data")
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "synthetic input")
	})
	text := receiveFileWorkMessage(t, p, got, fileWorkEvent("revision", "text", `{"text":"revise heading"}`, "group", "known-parent", false))
	if text.Content != "revise heading" || len(text.Files) != 0 || requests.Load() != 0 {
		t.Fatal("text revision changed or triggered a download")
	}
	file := receiveFileWorkMessage(t, p, got, fileWorkEvent("addition", "file", `{"file_key":"synthetic-key","file_name":"sample.xlsx"}`, "topic_group", "known-parent", false))
	if len(file.Files) != 1 || string(file.Files[0].Data) != "synthetic input" || !file.Files[0].RequireSave || file.Files[0].ReceiveError != "" {
		t.Fatal("owned attachment was not received through the controlled path")
	}
	if authorizationCalls.Load() != 2 || requests.Load() != 1 {
		t.Fatal("unexpected authorization or resource request count")
	}
}

func TestFileWorkReplyAuthorizationCarriesChatType(t *testing.T) {
	var seen []bool
	p, got := fileWorkFixture(t, func(msg core.Message, _ string) bool {
		seen = append(seen, msg.FileWorkPrivate)
		return true
	}, func(http.ResponseWriter, *http.Request) { t.Error("unexpected resource download") })
	for _, kind := range []string{"p2p", "group", "topic_group"} {
		receiveFileWorkMessage(t, p, got, fileWorkEvent(kind, "text", `{"text":"Revise this"}`, kind, "receipt", false))
	}
	if len(seen) != 3 || !seen[0] || seen[1] || seen[2] {
		t.Fatal("private reply entered group authorization", seen)
	}
	if err := p.onMessage(context.Background(), fileWorkEvent("unknown-kind", "text", `{"text":"Revise this"}`, "unknown", "receipt", true)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatal("unknown chat type acquired a group association")
	}
}

func TestGroupFileKnownUnimportableReplyReachesHintWithoutDownloads(t *testing.T) {
	p, got := fileWorkFixture(t, func(core.Message, string) bool { return false }, func(http.ResponseWriter, *http.Request) { t.Error("hint downloaded an attachment") })
	if err := p.SetFileWorkEnabled(true, func(_ core.Message, parent string) error {
		switch parent {
		case "known-task", "known-bot-text":
			return core.ErrFileWorkNeedsArtifact
		case "known-uncertain":
			return core.ErrFileWorkUnconfirmed
		default:
			return core.ErrFileWorkNotFound
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"known-task", "known-bot-text", "known-uncertain"} {
		m := receiveFileWorkMessage(t, p, got, fileWorkEvent("reply-"+parent, "file", `{"file_key":"must-not-fetch","file_name":"sample.docx"}`, "group", parent, false))
		if m.Content != "" || len(m.Files) != 0 {
			t.Fatal("hint path carried unselected material")
		}
	}
	if err := p.onMessage(context.Background(), fileWorkEvent("foreign", "file", `{"file_key":"must-not-fetch","file_name":"sample.docx"}`, "group", "foreign", false)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
		t.Fatal("unrelated unmentioned reply dispatched")
	default:
	}
}

func TestFileWorkUnknownParentAndOtherPrincipalNeverDownload(t *testing.T) {
	var authorizationCalls, requests atomic.Int32
	p, got := fileWorkFixture(t, func(core.Message, string) bool {
		authorizationCalls.Add(1)
		return false
	}, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(500)
	})
	for i, event := range []*larkim.P2MessageReceiveV1{
		fileWorkEvent("unknown-group", "file", `{"file_key":"unknown","file_name":"sample.docx"}`, "group", "unknown-parent", false),
		fileWorkEvent("unknown-topic", "text", `{"text":"unmentioned"}`, "topic_group", "unknown-parent", false),
		fileWorkEvent("other-sender", "file", `{"file_key":"unknown","file_name":"sample.docx"}`, "group", "known-parent", true),
		fileWorkEvent("other-chat", "file", `{"file_key":"unknown","file_name":"sample.docx"}`, "group", "known-parent", true),
	} {
		if i == 2 {
			event.Event.Sender.SenderId.OpenId = strPtr("other-sender")
		}
		if i == 3 {
			event.Event.Message.ChatId = strPtr("other-chat")
		}
		if err := p.onMessage(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-got:
		t.Fatal("unknown unmentioned or foreign event reached the host")
	default:
	}
	if authorizationCalls.Load() != 2 {
		t.Fatal("another principal reached the reply authorizer")
	}
	for _, chatType := range []string{"p2p", "group"} {
		msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("mentioned-unknown-"+chatType, "file", `{"file_key":"unknown","file_name":"sample.docx"}`, chatType, "unknown-parent", true))
		if msg.Content != "" || len(msg.Files) != 0 {
			t.Fatal("unknown parent carried content before host location check")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("unknown reply downloaded resources")
	}
}

func TestFileWorkStartsAndUnsupportedInputsNeverUseLegacyReaders(t *testing.T) {
	var requests atomic.Int32
	p, got := fileWorkFixture(t, func(core.Message, string) bool { return true }, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		if strings.Contains(r.URL.Path, "oversize") {
			w.Header().Set("Content-Length", strconv.Itoa(core.DefaultFileInputLimit+1))
			return
		}
		_, _ = io.WriteString(w, "synthetic file")
	})
	for _, tc := range []struct {
		name, kind, content string
		wantText            string
		wantError           core.MsgKey
		wantRequests        int32
	}{
		{name: "text", kind: "text", content: `{"text":"@_bot start"}`, wantText: "start"},
		{name: "post", kind: "post", content: `{"title":"Title","content":[[{"tag":"at","user_id":"bot-fixture"},{"tag":"text","text":"revise"}],[{"tag":"a","text":"reference","href":"https://example.invalid"},{"tag":"at","user_id":"other-user"}]]}`, wantText: "Title\nrevise\n[reference](https://example.invalid)\n@other-user"},
		{name: "localized-post", kind: "post", content: `{"zh_cn":{"title":"","content":[[{"tag":"markdown","text":"text only"}]]}}`, wantText: "text only"},
		{name: "file", kind: "file", content: `{"file_key":"sample","file_name":"sample.xlsx"}`, wantRequests: 1},
		{name: "oversize", kind: "file", content: `{"file_key":"oversize","file_name":"sample.xlsx"}`, wantError: core.MsgFileInputTooLarge, wantRequests: 1},
		{name: "broken-file", kind: "file", content: `{`, wantError: core.MsgFileInputUnavailable},
		{name: "missing-name", kind: "file", content: `{"file_key":"sample"}`, wantError: core.MsgFileInputUnavailable},
		{name: "broken-text", kind: "text", content: `{`, wantError: core.MsgFileInputUnavailable},
		{name: "image", kind: "image", content: `{"image_key":"sample"}`, wantError: core.MsgFileWorkUnavailable},
		{name: "audio", kind: "audio", content: `{"file_key":"sample"}`, wantError: core.MsgFileWorkUnavailable},
		{name: "merge", kind: "merge_forward", content: `null`, wantError: core.MsgFileWorkUnavailable},
		{name: "post-image", kind: "post", content: `{"content":[[{"tag":"text","text":"do not swallow this"},{"tag":"img","image_key":"sample"}]]}`, wantError: core.MsgFileWorkUnavailable},
		{name: "post-media", kind: "post", content: `{"content":[[{"tag":"media","file_key":"sample"}]]}`, wantError: core.MsgFileWorkUnavailable},
		{name: "hidden-locale-media", kind: "post", content: `{"en_us":{"content":[[{"tag":"text","text":"text"}]]},"zh_cn":{"content":[[{"tag":"img","image_key":"sample"}]]}}`, wantError: core.MsgFileWorkUnavailable},
		{name: "post-unknown", kind: "post", content: `{"content":[[{"tag":"future-attachment"}]]}`, wantError: core.MsgFileWorkUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := requests.Load()
			msg := receiveFileWorkMessage(t, p, got, fileWorkEvent(tc.name, tc.kind, tc.content, "group", "", true))
			if msg.Content != tc.wantText || requests.Load()-before != tc.wantRequests {
				t.Fatalf("content=%q requests=%d", msg.Content, requests.Load()-before)
			}
			if tc.wantError != "" {
				if len(msg.Files) != 1 || msg.Files[0].ReceiveError != tc.wantError || len(msg.Files[0].Data) != 0 || !msg.Files[0].RequireSave {
					t.Fatal("failed input was not explicitly rejected")
				}
			} else if tc.kind == "file" && (len(msg.Files) != 1 || len(msg.Files[0].Data) == 0 || !msg.Files[0].RequireSave) {
				t.Fatal("new file was not received")
			}
		})
	}
}

func TestFileWorkOptInRequiresFixedReceiverAndKeepsLegacyDefault(t *testing.T) {
	for _, mutate := range []func(*Platform){
		func(p *Platform) { p.strictRoutes = false },
		func(p *Platform) { p.allowFrom = "*" },
		func(p *Platform) { p.allowChat = "first,second" },
		func(p *Platform) { p.shareSessionInChannel = true },
		func(p *Platform) { p.threadIsolation = true },
	} {
		p := &Platform{strictRoutes: true, allowFrom: "sender", allowChat: "chat"}
		mutate(p)
		if p.SetFileWorkEnabled(true, func(core.Message, string) error { return nil }) == nil || p.fileWorkEnabled {
			t.Fatal("unsupported receiver enabled controlled file intake")
		}
	}
	p := &Platform{strictRoutes: true, allowFrom: "sender", allowChat: "chat"}
	if p.SetFileWorkEnabled(true, nil) == nil {
		t.Fatal("missing host authorizer accepted")
	}
	if p.fileWorkEnabled || p.fileReplyAuthorized != nil {
		t.Fatal("controlled intake enabled by default")
	}
	if err := p.SetFileWorkEnabled(true, func(core.Message, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.SetFileWorkEnabled(false, nil); err != nil || p.fileWorkEnabled || p.fileReplyAuthorized != nil {
		t.Fatal("host could not disable controlled intake")
	}
}
