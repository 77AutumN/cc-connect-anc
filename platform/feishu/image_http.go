package feishu

import (
	"io"
	"net/http"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// The SDK buffers binary responses before exposing File. Bound the response
// body before that allocation, not merely the reader returned by the SDK.
type imageBoundedHTTPClient struct{ client *http.Client }
type imageDownloadLimitKey struct{}
type imageReceiveBudget struct {
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
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/open-apis/im/v1/messages/") && strings.Contains(req.URL.Path, "/resources/") && req.URL.Query().Get("type") == "image" {
		limit := core.MaxImageBytes
		if remaining, ok := req.Context().Value(imageDownloadLimitKey{}).(int); ok {
			limit = min(limit, remaining)
		}
		if resp.ContentLength > int64(limit) {
			resp.Body.Close()
			return nil, &core.ImageInputError{Key: core.MsgImageLimit}
		}
		resp.Body = boundedImageBody{Reader: io.LimitReader(resp.Body, int64(limit)+1), Closer: resp.Body}
	}
	return resp, nil
}
