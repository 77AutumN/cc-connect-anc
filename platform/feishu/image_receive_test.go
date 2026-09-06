package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func imageReceiverFixture(t *testing.T, resource func(http.ResponseWriter, *http.Request)) *Platform {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "expire": 7200, "tenant_access_token": "synthetic-image-token"})
			return
		}
		resource(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Platform{platformName: "feishu", client: lark.NewClient(t.Name(), "synthetic-secret", lark.WithOpenBaseUrl(srv.URL), lark.WithHttpClient(imageBoundedHTTPClient{client: srv.Client()}))}
}

func TestImageBudgetSharedAcrossQuotedAndCurrentPosts(t *testing.T) {
	data := feishuImageFixture(t, 1)
	var calls atomic.Int32
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	})
	budget := &imageReceiveBudget{}
	post := `{"content":[[{"tag":"text","text":"before"},{"tag":"img","image_key":"fixture-a"},{"tag":"img","image_key":"fixture-b"},{"tag":"text","text":"after"}]]}`
	for i := 0; i < 2; i++ {
		parts, images := p.parsePostContent("fixture", post, budget)
		if core.CheckImageBatch(images) != nil || len(images) != 2 || !strings.HasPrefix(strings.Join(parts, ""), "before[attached-image:") {
			t.Fatal("image order/batch lost")
		}
	}
	_, images := p.parsePostContent("fixture", post, budget)
	if core.CheckImageBatch(images) == nil || calls.Load() != 4 {
		t.Fatal("quote/current aggregate exceeded four downloads")
	}
}

func TestImageRemainingBytesBoundedBeforeSDKBuffer(t *testing.T) {
	data := append(feishuImageFixture(t, 1), make([]byte, 4096)...)
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		w.Write(data)
	})
	budget := &imageReceiveBudget{count: 2, bytes: core.MaxImageBatchBytes - 128}
	img := p.receiveImage("fixture", "fixture", budget)
	if img.ReceiveError != core.MsgImageLimit || len(img.Data) != 0 {
		t.Fatal("partial remaining budget accepted")
	}
}

func TestImagePartialDownloadRejectsWholePostAndStops(t *testing.T) {
	data := feishuImageFixture(t, 1)
	var calls atomic.Int32
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "image/png")
		if calls.Load() == 2 {
			w.Header().Set("Content-Length", "999")
			w.Write(data[:8])
			return
		}
		w.Write(data)
	})
	post := `{"content":[[{"tag":"text","text":"must not send alone"},{"tag":"img","image_key":"a"},{"tag":"img","image_key":"b"},{"tag":"img","image_key":"c"}]]}`
	text, images := p.parsePostContent("fixture", post)
	if text != nil || core.CheckImageBatch(images) == nil || calls.Load() != 2 {
		t.Fatal("partial post continued after image failure")
	}
}

type imageBodyCounter struct {
	io.ReadCloser
	read int
}

func (c *imageBodyCounter) Read(p []byte) (int, error) {
	n, e := c.ReadCloser.Read(p)
	c.read += n
	return n, e
}

type imageRoundTrip func(*http.Request) (*http.Response, error)

func (f imageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestImageHTTPBoundLeavesOtherFilesUnchanged(t *testing.T) {
	for _, kind := range []string{"image", "file"} {
		body := &imageBodyCounter{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, 4096)))}
		client := imageBoundedHTTPClient{client: &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, ContentLength: -1, Body: body}, nil
		})}}
		ctx := context.WithValue(context.Background(), imageDownloadLimitKey{}, 128)
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://fixture/open-apis/im/v1/messages/fixture/resources/item?type="+kind, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		want := 4096
		if kind == "image" {
			want = 129
		}
		if body.read != want {
			t.Fatalf("%s buffered %d bytes, want %d", kind, body.read, want)
		}
	}
}

func TestImageRawBatchPreservesOrderBeforeDownloads(t *testing.T) {
	data := map[string][]byte{"a": feishuImageFixture(t, 1), "b": feishuImageFixture(t, 2)}
	var calls atomic.Int32
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(data[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	})
	var got *core.Message
	p.handler = func(_ core.Platform, m *core.Message) { got = m }
	p.imageBatchWindow = time.Hour
	p.bufferImage("fixture", &imageBatchEntry{sessionKey: "fixture", messageIDs: []string{"later"}, imageRefs: []imageBatchRef{{"later", "b", 2}}})
	p.bufferImage("fixture", &imageBatchEntry{sessionKey: "fixture", messageIDs: []string{"earlier"}, imageRefs: []imageBatchRef{{"earlier", "a", 1}}})
	if calls.Load() != 0 {
		t.Fatal("download started before coalescing raw image events")
	}
	p.flushImageBatchForSession("fixture")
	if got == nil || len(got.Images) != 2 || !bytes.Equal(got.Images[0].Data, data["a"]) || !bytes.Equal(got.Images[1].Data, data["b"]) {
		t.Fatal("multi-image order changed")
	}
}

func TestImageTimerDownloadCannotBeOvertakenByFollowingText(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	data := feishuImageFixture(t, 1)
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	})
	p.imageBatchWindow = time.Hour
	delivered := make(chan struct{})
	p.handler = func(_ core.Platform, m *core.Message) { close(delivered) }
	entry := &imageBatchEntry{sessionKey: "fixture", messageIDs: []string{"image"}, imageRefs: []imageBatchRef{{"image", "a", 1}}}
	p.bufferImage("fixture", entry)
	go p.flushImageBatchByRef("fixture", entry)
	<-started
	flushed := make(chan struct{})
	go func() { p.flushImageBatchForSession("fixture"); close(flushed) }()
	select {
	case <-flushed:
		t.Fatal("following text can overtake in-flight image download")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("following message remained blocked")
	}
	select {
	case <-delivered:
	default:
		t.Fatal("flush returned before image delivery")
	}
}
