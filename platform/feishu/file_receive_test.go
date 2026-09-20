package feishu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func requireFileInputError(t *testing.T, err error, key core.MsgKey) {
	t.Helper()
	var inputErr *core.FileInputError
	if !errors.As(err, &inputErr) || inputErr.Key != key {
		t.Fatalf("file input error = %v, want %s", err, key)
	}
}

type fileBodyCounter struct {
	io.Reader
	read   int
	closed bool
}

func (b *fileBodyCounter) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *fileBodyCounter) Close() error { b.closed = true; return nil }

func TestControlledFileHTTPBoundBeforeSDKBuffer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		marked     bool
		length     int64
		wantRead   int
		wantReject bool
	}{
		{name: "legacy file unchanged", length: -1, wantRead: 4096},
		{name: "marked chunked file", marked: true, length: -1, wantRead: 129},
		{name: "marked declared oversize", marked: true, length: 4096, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &fileBodyCounter{Reader: bytes.NewReader(make([]byte, 4096))}
			client := imageBoundedHTTPClient{client: &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, ContentLength: tc.length, Body: body}, nil
			})}}
			ctx := context.Background()
			if tc.marked {
				ctx = context.WithValue(ctx, fileDownloadLimitKey{}, int64(128))
			}
			req, err := http.NewRequestWithContext(ctx, "GET", "http://fixture/open-apis/im/v1/messages/fixture/resources/item?type=file", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if tc.wantReject {
				requireFileInputError(t, err, core.MsgFileInputTooLarge)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if body.read != tc.wantRead || !body.closed {
				t.Fatalf("body read=%d closed=%t, want read=%d closed=true", body.read, body.closed, tc.wantRead)
			}
		})
	}
}

func TestControlledFileDownloadRejectsPartialEmptyAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		length  string
		limit   int64
		wantErr core.MsgKey
	}{
		{name: "exact limit", payload: "synthetic", limit: 9},
		{name: "chunked oversize", payload: "synthetic", limit: 8, wantErr: core.MsgFileInputTooLarge},
		{name: "declared oversize", payload: "synthetic", length: "9", limit: 8, wantErr: core.MsgFileInputTooLarge},
		{name: "partial response", payload: "short", length: "99", limit: 128, wantErr: core.MsgFileInputUnavailable},
		{name: "empty response", limit: 128, wantErr: core.MsgFileInputUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, tc.payload)
			})
			data, err := p.downloadFileBounded(context.Background(), "synthetic-message", "synthetic-file", tc.limit)
			if tc.wantErr != "" {
				requireFileInputError(t, err, tc.wantErr)
				if data != nil {
					t.Fatal("failed download returned partial data")
				}
			} else if err != nil || string(data) != tc.payload {
				t.Fatalf("download = %q, %v", data, err)
			}
		})
	}
}

func TestControlledFileBatchRejectsPartialAndStopsFurtherDownloads(t *testing.T) {
	var calls atomic.Int32
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		if strings.HasSuffix(r.URL.Path, "/failed") {
			w.Header().Set("Content-Length", "99")
		}
		_, _ = io.WriteString(w, "synthetic")
	})
	budget := &fileReceiveBudget{}
	files, err := p.receiveFiles(context.Background(), []quotedFileMeta{
		{messageID: "message", fileKey: "first", fileName: "first.xlsx"},
		{messageID: "message", fileKey: "failed", fileName: "failed.xlsx"},
		{messageID: "message", fileKey: "third", fileName: "third.xlsx"},
	}, budget)
	requireFileInputError(t, err, core.MsgFileInputUnavailable)
	if files != nil || calls.Load() != 2 {
		t.Fatalf("failed batch returned %d files after %d downloads", len(files), calls.Load())
	}
	file := p.receiveFile(context.Background(), "other-message", "other-file", "other.xlsx", budget)
	if file.ReceiveError != core.MsgFileInputUnavailable || len(file.Data) != 0 || calls.Load() != 2 {
		t.Fatal("shared batch failure did not block later downloads")
	}
}

func TestControlledFileBudgetSharedAcrossCurrentQuotedAndForwarded(t *testing.T) {
	for _, countLimited := range []bool{false, true} {
		t.Run(strconv.FormatBool(countLimited), func(t *testing.T) {
			var calls atomic.Int32
			p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, "12345678")
			})
			budget := &fileReceiveBudget{bytes: maxFileInputBatchBytes - 12}
			wantCalls := int32(2)
			if countLimited {
				budget = &fileReceiveBudget{count: maxFileInputCount - 1}
				wantCalls = 1
			}
			first := p.receiveFile(context.Background(), "current", "file", "current.xlsx", budget)
			if first.ReceiveError != "" || !first.RequireSave || string(first.Data) != "12345678" {
				t.Fatal("valid file was not preserved for mandatory save")
			}
			files, err := p.receiveFiles(context.Background(), []quotedFileMeta{{messageID: "quoted", fileKey: "file", fileName: "quoted.xlsx"}}, budget)
			requireFileInputError(t, err, core.MsgFileInputTooLarge)
			if files != nil || calls.Load() != wantCalls {
				t.Fatal("aggregate budget was bypassed")
			}
			last := p.receiveFile(context.Background(), "forwarded", "file", "forwarded.xlsx", budget)
			if last.ReceiveError != core.MsgFileInputTooLarge || calls.Load() != wantCalls {
				t.Fatal("forwarded file reset a failed budget")
			}
		})
	}
}

func TestControlledFileDownloadCancellationAndInvalidReference(t *testing.T) {
	var calls atomic.Int32
	p := imageReceiverFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	})
	if data, err := p.downloadFileBounded(context.Background(), "", "file", 128); data != nil {
		t.Fatal("invalid reference returned data")
	} else {
		requireFileInputError(t, err, core.MsgFileInputUnavailable)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid reference reached resource API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	data, err := p.downloadFileBounded(ctx, "message", "file", 128)
	requireFileInputError(t, err, core.MsgFileInputUnavailable)
	if data != nil || ctx.Err() == nil {
		t.Fatal("cancelled download returned data or ignored context")
	}
}

func TestControlledFileDownloadAddsDeadlineAndRedactsAPIFailure(t *testing.T) {
	var resourceCalls int
	client := &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		payload := `{"code":0,"expire":7200,"tenant_access_token":"synthetic-token"}`
		if strings.Contains(r.URL.Path, "/resources/") {
			resourceCalls++
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Fatal("file download has no bounded deadline")
			}
			status = http.StatusForbidden
			payload = `{"code":999,"msg":"synthetic-private-detail"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}
	p := &Platform{platformName: "feishu", client: lark.NewClient(t.Name(), "synthetic-secret", lark.WithOpenBaseUrl("http://fixture"), lark.WithHttpClient(imageBoundedHTTPClient{client: client}))}
	data, err := p.downloadFileBounded(context.Background(), "message", "file", 128)
	requireFileInputError(t, err, core.MsgFileInputUnavailable)
	if data != nil || resourceCalls != 1 || strings.Contains(err.Error(), "synthetic-private-detail") {
		t.Fatal("API failure leaked raw details or triggered a retry")
	}
}
