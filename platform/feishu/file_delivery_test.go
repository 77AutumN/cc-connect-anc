package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type fileDeliveryObserved struct {
	mu      sync.Mutex
	uploads [][]byte
	paths   []string
	bodies  []struct {
		UUID          string `json:"uuid"`
		ReceiveID     string `json:"receive_id"`
		ReplyInThread bool   `json:"reply_in_thread"`
		Content       string `json:"content"`
	}
	redirects int
}

func fileDeliveryFixture(t *testing.T, outcome string) (*Platform, *fileDeliveryObserved) {
	t.Helper()
	observed := &fileDeliveryObserved{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			writeJSON(t, w, map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "synthetic-token"})
			return
		}
		observed.mu.Lock()
		defer observed.mu.Unlock()
		if r.URL.Path == "/redirect-target" {
			observed.redirects++
			writeJSON(t, w, map[string]any{"code": 0})
			return
		}
		if r.URL.Path == "/open-apis/im/v1/files" {
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer func() { _ = file.Close() }()
			if r.MultipartForm != nil {
				defer func() { _ = r.MultipartForm.RemoveAll() }()
			}
			data, err := io.ReadAll(file)
			if err != nil {
				t.Error(err)
			}
			observed.uploads = append(observed.uploads, data)
			if outcome == "upload_rejected" {
				writeJSON(t, w, map[string]any{"code": 230001, "msg": "synthetic-private-detail"})
				return
			}
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"file_key": "synthetic-file-key"}})
			return
		}
		observed.paths = append(observed.paths, r.URL.Path)
		observed.bodies = append(observed.bodies, struct {
			UUID          string `json:"uuid"`
			ReceiveID     string `json:"receive_id"`
			ReplyInThread bool   `json:"reply_in_thread"`
			Content       string `json:"content"`
		}{})
		if err := json.NewDecoder(r.Body).Decode(&observed.bodies[len(observed.bodies)-1]); err != nil {
			t.Error(err)
		}
		switch outcome {
		case "deleted_origin":
			writeJSON(t, w, map[string]any{"code": 230011, "msg": "synthetic-private-detail"})
		case "missing_receipt":
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{}})
		case "server_failure":
			w.WriteHeader(503)
			writeJSON(t, w, map[string]any{"code": 999, "msg": "synthetic-private-detail"})
		case "malformed_response":
			_, _ = io.WriteString(w, "{")
		case "redirect":
			w.Header().Set("Location", "/redirect-target")
			w.WriteHeader(http.StatusTemporaryRedirect)
			writeJSON(t, w, map[string]any{"code": 0})
		default:
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"message_id": "accepted-file"}})
		}
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	if outcome == "lost_response" || outcome == "lost_upload_response" {
		base := client.Transport
		client.Transport = imageRoundTrip(func(r *http.Request) (*http.Response, error) {
			resp, err := base.RoundTrip(r)
			lost := strings.HasSuffix(r.URL.Path, "/reply") || r.URL.Path == "/open-apis/im/v1/messages"
			if outcome == "lost_upload_response" {
				lost = r.URL.Path == "/open-apis/im/v1/files"
			}
			if err == nil && lost {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				return nil, io.ErrUnexpectedEOF
			}
			return resp, err
		})
	}
	p := &Platform{platformName: "feishu", appID: t.Name(), strictRoutes: true, allowFrom: "sender-fixture", allowChat: "chat-fixture"}
	p.client = lark.NewClient(p.appID, "synthetic-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(imageBoundedHTTPClient{client: client}))
	p.sharedGroup = &sharedWSGroup{platforms: []*Platform{p}}
	return p, observed
}

func fileDeliverySource() replyContext {
	return replyContext{messageID: "origin-message", chatID: "chat-fixture", sessionKey: "feishu:chat-fixture:sender-fixture"}
}

func TestFileDeliveryPinsOriginalRouteUUIDAndSnapshot(t *testing.T) {
	for _, mode := range []string{"reply", "create", "quote", "topic"} {
		t.Run(mode, func(t *testing.T) {
			p, observed := fileDeliveryFixture(t, "success")
			source := fileDeliverySource()
			p.noReplyToTrigger = mode != "reply"
			if mode == "topic" {
				source.rootID, source.threadID = "original-root", "original-thread"
			}
			if mode == "quote" {
				source.rootID = "original-root"
			}
			route, err := p.FileReplyRoute(source)
			if err != nil {
				t.Fatal(err)
			}
			source.messageID, source.chatID = "later-message", "other-chat"
			p.noReplyToTrigger = !p.noReplyToTrigger // Frozen policy must not drift.
			file := core.FileAttachment{FileName: "never-read-from-disk.xlsx", Data: []byte("immutable synthetic snapshot")}
			id, err := p.SendFileWithReceipt(context.Background(), route, file, "fixed-delivery-id")
			if err != nil || id != "accepted-file" {
				t.Fatalf("receipt = %q, %v", id, err)
			}
			observed.mu.Lock()
			defer observed.mu.Unlock()
			if len(observed.uploads) != 1 || !bytes.Equal(observed.uploads[0], file.Data) || len(observed.paths) != 1 {
				t.Fatal("snapshot changed or request repeated")
			}
			body := observed.bodies[0]
			wantPath := "/open-apis/im/v1/messages/origin-message/reply"
			if mode == "create" || mode == "quote" {
				wantPath = "/open-apis/im/v1/messages"
				if body.ReceiveID != "chat-fixture" {
					t.Fatal("create destination changed")
				}
			}
			if observed.paths[0] != wantPath || body.UUID != "fixed-delivery-id" || body.ReplyInThread != (mode == "topic") || !strings.Contains(body.Content, "synthetic-file-key") {
				t.Fatal("original route, UUID or file reference changed")
			}
		})
	}
}

func TestFileDeliveryUnknownNeverRetriesOrFallsBack(t *testing.T) {
	for _, outcome := range []string{"lost_response", "missing_receipt", "server_failure", "malformed_response", "redirect", "deleted_origin", "upload_rejected", "lost_upload_response"} {
		t.Run(outcome, func(t *testing.T) {
			p, observed := fileDeliveryFixture(t, outcome)
			source := fileDeliverySource()
			source.rootID, source.threadID = "original-root", "original-topic"
			route, err := p.FileReplyRoute(source)
			if err != nil {
				t.Fatal(err)
			}
			id, err := p.SendFileWithReceipt(context.Background(), route, core.FileAttachment{FileName: "sample.docx", Data: []byte("synthetic")}, "fixed-delivery-id")
			wantNotSubmitted := outcome == "deleted_origin" || outcome == "upload_rejected" || outcome == "lost_upload_response"
			if err == nil || id != "" || errors.Is(err, core.ErrFileNotSubmitted) != wantNotSubmitted || strings.Contains(err.Error(), "synthetic-private-detail") {
				t.Fatalf("incorrect acceptance category: %q, %v", id, err)
			}
			observed.mu.Lock()
			defer observed.mu.Unlock()
			wantSends := 1
			if strings.Contains(outcome, "upload") {
				wantSends = 0
			}
			if len(observed.uploads) != 1 || len(observed.paths) != wantSends || observed.redirects != 0 {
				t.Fatalf("uploads=%d sends=%d redirects=%d", len(observed.uploads), len(observed.paths), observed.redirects)
			}
			if wantSends == 1 && observed.paths[0] != "/open-apis/im/v1/messages/origin-message/reply" {
				t.Fatal("failed topic reply fell back to another destination")
			}
		})
	}
}

func TestFileDeliveryRejectsInvalidAndReboundRoutesBeforeUpload(t *testing.T) {
	p, observed := fileDeliveryFixture(t, "success")
	route, err := p.FileReplyRoute(fileDeliverySource())
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		`{}`, string(route) + `{}`, `null`,
		strings.TrimSuffix(string(route), "}") + `,"unknown":true}`,
		strings.TrimSuffix(string(route), "}") + `,"chat_id":"chat-fixture"}`,
		strings.Replace(string(route), `"reply":true`, `"reply":null`, 1),
		strings.Replace(string(route), `"version":1`, `"version":2`, 1),
		strings.Replace(string(route), `"sender_id":"sender-fixture"`, `"sender_id":"another-sender"`, 1),
		strings.Replace(string(route), `"chat_id":"chat-fixture"`, `"chat_id":"another-chat"`, 1),
		strings.Replace(string(route), `"thread_id":""`, `"thread_id":"topic-without-thread-flag"`, 1),
	} {
		if _, err := p.SendFileWithReceipt(context.Background(), json.RawMessage(invalid), core.FileAttachment{FileName: "sample.xlsx", Data: []byte("synthetic")}, "delivery"); !errors.Is(err, core.ErrFileNotSubmitted) {
			t.Fatal("invalid route reached upload")
		}
	}
	p.appID = "changed-application"
	if _, err := p.SendFileWithReceipt(context.Background(), route, core.FileAttachment{FileName: "sample.xlsx", Data: []byte("synthetic")}, "delivery"); !errors.Is(err, core.ErrFileNotSubmitted) {
		t.Fatal("route survived application rebinding")
	}
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if len(observed.uploads) != 0 || len(observed.paths) != 0 {
		t.Fatal("invalid route caused external effects")
	}
}

func TestFileReplyRouteRequiresFixedVerifiedSession(t *testing.T) {
	p, _ := fileDeliveryFixture(t, "success")
	for _, source := range []any{"not-a-reply-context", replyContext{}, replyContext{messageID: "origin", chatID: "chat-fixture", sessionKey: "feishu:chat-fixture:another-sender"}} {
		if _, err := p.FileReplyRoute(source); !errors.Is(err, core.ErrFileNotSubmitted) {
			t.Fatal("unbound reply context accepted")
		}
	}
	p.strictRoutes = false
	if _, err := p.FileReplyRoute(fileDeliverySource()); !errors.Is(err, core.ErrFileNotSubmitted) {
		t.Fatal("unrestricted platform accepted a delivery route")
	}
	p.strictRoutes = true
	p.sharedGroup.platforms = append(p.sharedGroup.platforms, &Platform{strictRoutes: true, allowFrom: p.allowFrom, allowChat: p.allowChat})
	if _, err := p.FileReplyRoute(fileDeliverySource()); !errors.Is(err, core.ErrFileNotSubmitted) {
		t.Fatal("ambiguous fixed route accepted a delivery route")
	}
}

func TestLegacySendFileKeepsExistingCreateBehavior(t *testing.T) {
	p, observed := fileDeliveryFixture(t, "success")
	p.noReplyToTrigger = true
	if err := p.SendFile(context.Background(), fileDeliverySource(), core.FileAttachment{FileName: "sample.xlsx", Data: []byte("synthetic")}); err != nil {
		t.Fatal(err)
	}
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if len(observed.uploads) != 1 || len(observed.paths) != 1 || observed.paths[0] != "/open-apis/im/v1/messages" || observed.bodies[0].UUID != "" {
		t.Fatal("legacy upload or send behavior changed")
	}
}
