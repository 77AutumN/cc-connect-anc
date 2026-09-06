package core

import (
	"errors"
	"io"
	"net/http"
)

const (
	MaxImageCount      = 4
	MaxImageBytes      = 5 << 20
	MaxImageBatchBytes = 10 << 20
)

// ImageInputError carries only a localized failure category, never input data.
type ImageInputError struct{ Key MsgKey }

func (e *ImageInputError) Error() string { return NewI18n(LangEnglish).T(e.Key) }

func CheckImageBatch(images []ImageAttachment) error {
	if len(images) > MaxImageCount {
		return &ImageInputError{MsgImageLimit}
	}
	total := 0
	for _, img := range images {
		if img.ReceiveError != "" {
			return &ImageInputError{img.ReceiveError}
		}
		if len(img.Data) > MaxImageBytes {
			return &ImageInputError{MsgImageLimit}
		}
		total += len(img.Data)
		if total > MaxImageBatchBytes {
			return &ImageInputError{MsgImageLimit}
		}
	}
	return nil
}

// ReadImage bounds allocation even when the server omits Content-Length.
func ReadImage(r io.Reader) ([]byte, string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxImageBytes+1))
	if err != nil {
		return nil, "", &ImageInputError{MsgImageReceiveFailed}
	}
	if len(data) > MaxImageBytes {
		return nil, "", &ImageInputError{MsgImageLimit}
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return data, mime, nil
	default:
		return nil, "", &ImageInputError{MsgImageInvalid}
	}
}

func ImageErrorMessage(err error, i18n *I18n) (string, bool) {
	var inputErr *ImageInputError
	if errors.As(err, &inputErr) {
		return i18n.T(inputErr.Key), true
	}
	return "", false
}
