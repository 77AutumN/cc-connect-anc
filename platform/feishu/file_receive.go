package feishu

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	maxFileInputCount      = 4
	maxFileInputBatchBytes = 2 * core.DefaultFileInputLimit
)

type fileDownloadLimitKey struct{}

// A controlled dispatch shares one budget across current, quoted and forwarded
// files. These helpers are not enabled on the legacy inbound paths.
type fileReceiveBudget struct {
	count   int
	bytes   int64
	failure core.MsgKey
}

func (p *Platform) downloadFileBounded(ctx context.Context, messageID, key string, limit int64) ([]byte, error) {
	return p.downloadResourceBounded(ctx, messageID, key, "file", limit)
}

func (p *Platform) downloadResourceBounded(ctx context.Context, messageID, key, kind string, limit int64) ([]byte, error) {
	if messageID == "" || key == "" {
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	if limit <= 0 || limit > core.DefaultFileInputLimit {
		return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, fileDownloadLimitKey{}, limit)
	resp, err := p.client.Im.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).FileKey(key).Type(kind).Build())
	if err != nil {
		var inputErr *core.FileInputError
		if errors.As(err, &inputErr) {
			return nil, inputErr
		}
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	if resp == nil || !resp.Success() || resp.File == nil {
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, limit+1))
	if closer, ok := resp.File.(io.Closer); ok {
		if closeErr := closer.Close(); closeErr != nil {
			return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
		}
	}
	if int64(len(data)) > limit {
		return nil, core.NewFileInputError(core.MsgFileInputTooLarge)
	}
	if err != nil || len(data) == 0 || ctx.Err() != nil {
		return nil, core.NewFileInputError(core.MsgFileInputUnavailable)
	}
	return data, nil
}

func (p *Platform) receiveFile(ctx context.Context, messageID, key, name string, budget *fileReceiveBudget) core.FileAttachment {
	return p.receiveResource(ctx, messageID, key, name, "file", budget)
}

func (p *Platform) receiveResource(ctx context.Context, messageID, key, name, kind string, budget *fileReceiveBudget) core.FileAttachment {
	file := core.FileAttachment{FileName: name, RequireSave: true}
	if budget == nil {
		budget = &fileReceiveBudget{}
	}
	if budget.failure == "" && (budget.count >= maxFileInputCount || budget.bytes >= maxFileInputBatchBytes) {
		budget.failure = core.MsgFileInputTooLarge
	}
	if budget.failure != "" {
		file.ReceiveError = budget.failure
		return file
	}
	budget.count++
	data, err := p.downloadResourceBounded(ctx, messageID, key, kind, min(core.DefaultFileInputLimit, maxFileInputBatchBytes-budget.bytes))
	if err != nil {
		budget.failure = core.MsgFileInputUnavailable
		var inputErr *core.FileInputError
		if errors.As(err, &inputErr) {
			budget.failure = inputErr.Key
		}
		file.ReceiveError = budget.failure
		return file
	}
	budget.bytes += int64(len(data))
	file.Data, file.MimeType = data, detectMimeType(data)
	return file
}

// receiveFiles requires already-authorized references (for quoted files, call
// filterQuotedFilesForUser first). One failed file rejects the whole batch.
func (p *Platform) receiveFiles(ctx context.Context, metas []quotedFileMeta, budget *fileReceiveBudget) ([]core.FileAttachment, error) {
	if budget == nil {
		budget = &fileReceiveBudget{}
	}
	if budget.failure != "" {
		return nil, core.NewFileInputError(budget.failure)
	}
	var files []core.FileAttachment
	for _, meta := range metas {
		file := p.receiveFile(ctx, meta.messageID, meta.fileKey, meta.fileName, budget)
		if file.ReceiveError != "" {
			return nil, core.NewFileInputError(file.ReceiveError)
		}
		files = append(files, file)
	}
	return files, nil
}
