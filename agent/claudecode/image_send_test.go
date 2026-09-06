package claudecode

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type imageTestStdin struct{ bytes.Buffer }

func (*imageTestStdin) Close() error { return nil }

func fixtureImage(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestSendImageCacheFailureNeverSendsTextOnly(t *testing.T) {
	dir := t.TempDir()
	// A regular file makes the attachment path unusable on every OS, even root.
	if err := os.WriteFile(filepath.Join(dir, ".cc-connect"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	stdin := &imageTestStdin{}
	cs := &claudeSession{workDir: dir, stdin: stdin, ctx: context.Background()}
	cs.alive.Store(true)
	err := cs.Send("Read every attached image", "fixture-message", []core.ImageAttachment{{MimeType: "image/png", Data: fixtureImage(t)}}, nil)
	if err == nil || stdin.Len() != 0 {
		t.Fatalf("cache failure must reject the whole message: err=%v, stdin_bytes=%d", err, stdin.Len())
	}
}
