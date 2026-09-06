package claudecode

import (
	"bytes"
	"context"
	"github.com/chenhg5/cc-connect/core"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageLeasesReleasedAfterNativeProcessFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := helperCommand(ctx, "fail-before-result")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var released atomic.Int32
	cs := &claudeSession{cmd: cmd, ctx: ctx, cancel: cancel, events: make(chan core.Event, 64), done: make(chan struct{}), imageReleases: []func(){func() { released.Add(1) }}}
	cs.alive.Store(true)
	finished := make(chan struct{})
	go func() { cs.readLoop(stdout, &stderr); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("native failure did not finish")
	}
	if released.Load() != 1 {
		t.Fatal("failed native process pinned images")
	}
	cs.releaseImages()
	if released.Load() != 1 {
		t.Fatal("lease released twice")
	}
}
