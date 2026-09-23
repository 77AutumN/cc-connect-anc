package files

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"path"
	"strings"
	"time"
)

// SetDocumentFormats is a startup-only opt-in. The fixed executable is supplied
// by the host configuration, never by a tool argument or a received document.
// It must implement the CRM team_file_pdf.py stdin/exit-status contract.
func (h *Host) SetDocumentFormats(validator string) error {
	if ValidateHostFile(validator) != nil {
		return ErrUnavailable
	}
	h.documentValidator = validator
	return nil
}

func (h *Host) validateFile(ctx context.Context, name string, data []byte) error {
	if h.documentValidator == "" {
		return validateOOXML(name, data)
	}
	if len(data) == 0 || len(data) > MaxFileBytes {
		return ErrInvalid
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg":
		return validateImage(name, data)
	case ".pdf":
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, h.documentValidator)
		cmd.Stdin = bytes.NewReader(data)
		// No inherited Python startup hooks, credentials, document paths or logs.
		cmd.Env = []string{"LANG=C.UTF-8"}
		cmd.WaitDelay = time.Second
		var result bytes.Buffer
		cmd.Stdout = &result
		if err := cmd.Run(); err != nil {
			var rejected *exec.ExitError
			if ctx.Err() == nil && errors.As(err, &rejected) && rejected.ExitCode() == 1 {
				return ErrInvalid
			}
			slog.Warn("file work: PDF validator execution unavailable", "timed_out", ctx.Err() != nil)
			return ErrUnavailable
		}
		if result.String() != "PDF_OK_V1\n" {
			slog.Warn("file work: PDF validator returned an invalid protocol response")
			return ErrUnavailable
		}
		return nil
	default:
		return validateOffice(name, data, true)
	}
}
