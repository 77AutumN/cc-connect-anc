package feishu

import (
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// The SDK buffers binary responses before exposing File. Bound the response
// body before that allocation, not merely the reader returned by the SDK.
type imageBoundedHTTPClient struct{ client *http.Client }
type imageDownloadLimitKey struct{}
type imageReceiveBudget struct {
	chatID       string // Actual receiving chat, for quoted-message privacy.
	count, bytes int
	failure      core.MsgKey
}

// One dispatch owns its budget, including quoted and forwarded images.
func imageBudget(budgets []*imageReceiveBudget) *imageReceiveBudget {
	if len(budgets) > 0 && budgets[0] != nil {
		return budgets[0]
	}
	return &imageReceiveBudget{}
}

type boundedImageBody struct {
	io.Reader
	io.Closer
}

func (c imageBoundedHTTPClient) Do(req *http.Request) (*http.Response, error) {
	client := c.client
	if controlled, _ := req.Context().Value(fileDeliveryRequestKey{}).(bool); controlled {
		copy := *client
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &copy
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	// File limits are opt-in and host-owned. Unmarked legacy file requests
	// retain their existing behavior; controlled requests are bounded before
	// the SDK buffers the body, including chunked responses.
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/open-apis/im/v1/messages/") && strings.Contains(req.URL.Path, "/resources/") && req.URL.Query().Get("type") == "file" {
		if limit, ok := req.Context().Value(fileDownloadLimitKey{}).(int64); ok {
			if limit <= 0 || limit > core.DefaultFileInputLimit || resp.ContentLength > limit {
				if err := resp.Body.Close(); err != nil {
					slog.Warn("feishu: rejected file response close failed")
				}
				return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
			}
			resp.Body = boundedImageBody{Reader: io.LimitReader(resp.Body, limit+1), Closer: resp.Body}
		}
	}
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/open-apis/im/v1/messages/") && strings.Contains(req.URL.Path, "/resources/") && req.URL.Query().Get("type") == "image" {
		limit := core.MaxImageBytes
		if remaining, ok := req.Context().Value(imageDownloadLimitKey{}).(int); ok {
			limit = min(limit, remaining)
		}
		if resp.ContentLength > int64(limit) {
			if err := resp.Body.Close(); err != nil {
				slog.Warn("feishu: rejected image response close failed")
			}
			return nil, &core.ImageInputError{Key: core.MsgImageLimit}
		}
		resp.Body = boundedImageBody{Reader: io.LimitReader(resp.Body, int64(limit)+1), Closer: resp.Body}
	}
	return resp, nil
}
