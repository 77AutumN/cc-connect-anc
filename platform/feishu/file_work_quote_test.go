package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestFileWorkDesktopStandaloneFileCanBeExplicitlySelected(t *testing.T) {
	var metadata, downloads atomic.Int32
	p, got := fileWorkFixture(t, func(core.Message, string) bool { return false }, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/open-apis/im/v1/messages/uploaded-file" {
			metadata.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[{"message_id":"uploaded-file","chat_id":"chat-fixture","msg_type":"file","sender":{"id":"sender-fixture","id_type":"open_id","sender_type":"user"},"body":{"content":"{\"file_key\":\"fixture-file\",\"file_name\":\"fictional.pdf\"}"}}]}}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/messages/uploaded-file/resources/fixture-file") {
			t.Error("unexpected resource", r.URL.Path)
		}
		downloads.Add(1)
		_, _ = io.WriteString(w, "fictional PDF bytes; host validates the actual format")
	})
	if err := p.SetFileWorkImagesEnabled(true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, content string }{
		{"text", `{"text":"@_bot Prepare a proposal using this file"}`},
		{"post", `{"content":[[{"tag":"text","text":"@_bot Prepare a proposal using this file"}]]}`},
	} {
		msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("request-"+tc.kind, tc.kind, tc.content, "group", "uploaded-file", true))
		if msg.Content != "Prepare a proposal using this file" || len(msg.Files) != 1 || !msg.Files[0].RequireSave ||
			!msg.FileWorkNewInput || msg.Files[0].FileName != "fictional.pdf" {
			t.Fatal("the employee's explicitly quoted standalone upload was not selected", tc.kind)
		}
	}
	if metadata.Load() != 2 || downloads.Load() != 2 {
		t.Fatal("expected one metadata read and resource per selected message")
	}
}

func TestFileWorkQuotedUploadKeepsSelectionBoundary(t *testing.T) {
	for _, mode := range []string{"other-sender", "other-chat", "bot", "wrong-id", "wrong-id-type", "deleted", "text", "bad-json", "extra-item", "oversize-metadata", "disabled", "known", "unconfirmed", "unmentioned", "private", "empty-instruction", "post-extra-image", "recalled", "recall-during-download", "download-failure"} {
		t.Run(mode, func(t *testing.T) {
			var metadata, downloads atomic.Int32
			var p *Platform
			var got <-chan *core.Message
			p, got = fileWorkFixture(t, func(core.Message, string) bool { return mode == "known" }, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/resources/") {
					downloads.Add(1)
					if mode == "recall-during-download" {
						p.markMessageRecalled("upload")
						_, _ = io.WriteString(w, "fictional bytes")
						return
					}
					w.WriteHeader(500)
					return
				}
				metadata.Add(1)
				w.Header().Set("Content-Type", "application/json")
				item := map[string]any{"message_id": "upload", "chat_id": "chat-fixture", "msg_type": "file", "sender": map[string]any{"id": "sender-fixture", "id_type": "open_id", "sender_type": "user"}, "body": map[string]any{"content": `{"file_key":"fixture","file_name":"fictional.pdf"}`}}
				sender := item["sender"].(map[string]any)
				switch mode {
				case "other-sender":
					sender["id"] = "other"
				case "other-chat":
					item["chat_id"] = "other"
				case "bot":
					sender["sender_type"] = "app"
				case "wrong-id":
					item["message_id"] = "other"
				case "wrong-id-type":
					sender["id_type"] = "union_id"
				case "deleted":
					item["deleted"] = true
				case "text":
					item["msg_type"] = "text"
				case "bad-json":
					_, _ = io.WriteString(w, "{")
					return
				case "oversize-metadata":
					_, _ = io.WriteString(w, strings.Repeat(" ", 64<<10+1))
					return
				}
				items := []any{item}
				if mode == "extra-item" {
					items = append(items, item)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": items}})
			})
			if mode != "disabled" {
				_ = p.SetFileWorkImagesEnabled(true)
			}
			if mode == "unconfirmed" {
				_ = p.SetFileWorkEnabled(true, func(core.Message, string) error { return core.ErrFileWorkUnconfirmed })
			}
			chat, kind, body := "group", "text", `{"text":"@_bot use this file"}`
			if mode == "recalled" {
				p.markMessageRecalled("upload")
			}
			if mode == "post-extra-image" {
				kind, body = "post", `{"content":[[{"tag":"text","text":"@_bot use this"},{"tag":"img","image_key":"unselected"}]]}`
			}
			if mode == "private" {
				chat = "p2p"
			}
			if mode == "empty-instruction" {
				body = `{"text":"@_bot "}`
			}
			event := fileWorkEvent("request", kind, body, chat, "upload", mode != "unmentioned")
			if mode == "unmentioned" {
				_ = p.onMessage(context.Background(), event)
				select {
				case <-got:
					t.Fatal("unmentioned upload dispatched")
				default:
				}
			} else {
				msg := receiveFileWorkMessage(t, p, got, event)
				if mode == "download-failure" {
					if !msg.FileWorkNewInput || len(msg.Files) != 1 || msg.Files[0].ReceiveError != core.MsgFileInputUnavailable {
						t.Fatal("download failure disappeared")
					}
				} else if msg.FileWorkNewInput || len(msg.Files) != 0 {
					t.Fatal("unselected or foreign material reached host")
				}
			}
			wantMetadata, wantDownloads := int32(1), int32(0)
			if mode == "disabled" || mode == "known" || mode == "unconfirmed" || mode == "unmentioned" || mode == "private" || mode == "empty-instruction" || mode == "post-extra-image" || mode == "recalled" {
				wantMetadata = 0
			}
			if mode == "download-failure" || mode == "recall-during-download" {
				wantDownloads = 1
			}
			if metadata.Load() != wantMetadata || downloads.Load() != wantDownloads {
				t.Fatal("unexpected fetch", metadata.Load(), downloads.Load())
			}
		})
	}
}

func TestFileWorkQuotedInputNeverOverridesHostUncertainty(t *testing.T) {
	// A host error other than not-found must never enable fallback retrieval.
	p, got := fileWorkFixture(t, func(core.Message, string) bool { return false }, func(http.ResponseWriter, *http.Request) { t.Error("unexpected download") })
	_ = p.SetFileWorkImagesEnabled(true)
	_ = p.SetFileWorkEnabled(true, func(core.Message, string) error { return errors.New("storage unavailable") })
	msg := receiveFileWorkMessage(t, p, got, fileWorkEvent("request", "text", `{"text":"@_bot use this"}`, "group", "upload", true))
	if msg.FileWorkNewInput || len(msg.Files) != 0 {
		t.Fatal("host failure bypassed")
	}
}
