package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestSendControlledFileFailureNeverSendsTextOnly(t *testing.T) {
	for _, receiveFailure := range []bool{false, true} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".cc-connect"), []byte("blocked"), 0600); err != nil {
			t.Fatal(err)
		}
		stdin := &imageTestStdin{}
		cs := &claudeSession{workDir: dir, stdin: stdin, ctx: context.Background()}
		cs.alive.Store(true)
		file := core.FileAttachment{FileName: "fictional.xlsx", Data: []byte("fixture"), RequireSave: true}
		if receiveFailure {
			file.ReceiveError = core.MsgFileInputUnavailable
		}
		err := cs.Send("Organize this fictional file", "fixture-message", nil, []core.FileAttachment{file})
		var inputErr *core.FileInputError
		if !errors.As(err, &inputErr) || stdin.Len() != 0 {
			t.Fatalf("failed attachment reached model: err=%v bytes=%d", err, stdin.Len())
		}
	}
}
