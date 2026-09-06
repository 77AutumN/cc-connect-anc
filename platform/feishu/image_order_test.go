package feishu

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func imageInputEvent(kind, id, content, parent string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender:  &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strPtr("fixture-user")}, SenderType: strPtr("user")},
		Message: &larkim.EventMessage{MessageId: &id, ChatId: strPtr("fixture-chat"), ChatType: strPtr("p2p"), MessageType: &kind, Content: &content, ParentId: &parent},
	}}
}

func TestImagePostAndQuoteCannotBeOvertaken(t *testing.T) {
	for _, kind := range []string{"post", "quoted-image", "quoted-text"} {
		t.Run(kind, func(t *testing.T) {
			started, release := make(chan struct{}, 2), make(chan struct{})
			data := feishuImageFixture(t, 1)
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/resources/") {
					started <- struct{}{}
					<-release
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(data)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"items": []any{map[string]any{"msg_type": "image", "body": map[string]string{"content": `{"image_key":"quoted"}`}}}}})
			})
			p.dedup = &core.MessageDedup{}
			got := make(chan *core.Message, 3)
			p.handler = func(_ core.Platform, m *core.Message) { got <- m }
			messageType, parent, content := "post", "", `{"content":[[{"tag":"text","text":"before"},{"tag":"img","image_key":"first"},{"tag":"text","text":"after"}]]}`
			if kind == "quoted-image" {
				messageType, parent, content = "image", "quoted", `{"image_key":"first"}`
			}
			if kind == "quoted-text" {
				messageType, parent, content = "text", "quoted", `{"text":"identify this"}`
			}
			if err := p.onMessage(context.Background(), imageInputEvent(messageType, "first", content, parent)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("image download did not start")
			}
			if err := p.onMessage(context.Background(), imageInputEvent("text", "second", `{"text":"continue"}`, "")); err != nil {
				t.Fatal(err)
			}
			var early *core.Message
			select {
			case early = <-got:
			case <-time.After(40 * time.Millisecond):
			}
			close(release)
			if early != nil {
				t.Fatalf("later message overtook images: %s", early.MessageID)
			}
			for _, id := range []string{"first", "second"} {
				select {
				case msg := <-got:
					if msg.MessageID != id {
						t.Fatalf("got %s before %s", msg.MessageID, id)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("dispatch stalled")
				}
			}
		})
	}
}

func TestImageQuoteMetadataFailureRejectsWholeInput(t *testing.T) {
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":999,"msg":"fixture unavailable"}`))
	})
	quote := p.fetchQuotedMessage(context.Background(), "missing")
	if core.CheckImageBatch(quote.images) == nil || quote.text != "" {
		t.Fatal("unavailable quote silently omitted")
	}
}

func TestImageStickerAndThumbnailFailureIsNotSilent(t *testing.T) {
	for _, kind := range []string{"sticker", "media"} {
		t.Run(kind, func(t *testing.T) {
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":999}`))
			})
			var got *core.Message
			p.handler = func(_ core.Platform, m *core.Message) { got = m }
			p.dispatchMessage(context.Background(), kind, `{"file_key":"fixture","image_key":"fixture"}`, nil, "fixture", "fixture", "", "", replyContext{}, "", 0)
			if got == nil || core.CheckImageBatch(got.Images) == nil {
				t.Fatal("image download failure became successful text")
			}
		})
	}
}
