package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type blockingSupplementHost struct {
	*Host
	entered, release chan struct{}
}

func (h *blockingSupplementHost) Bind(ctx context.Context, b Binding) (WorkContext, error) {
	if b.DeferInputs {
		close(h.entered)
		<-h.release
	}
	return h.Host.Bind(ctx, b)
}

func TestCUJ_FileSupplement_TurnFinishesDuringIntake(t *testing.T) {
	p := &fileJourneyPlatform{}
	f := newFixture(t, p.SendFileWithReceipt)
	base := t.TempDir()
	_ = os.Chmod(base, 0700)
	path := filepath.Join(t.TempDir(), "sessions.json")
	shared := contextOnlyJourney()
	e := conversationEngine(t, f, p, shared, base, path)
	host := &blockingSupplementHost{Host: f.host, entered: make(chan struct{}), release: make(chan struct{})}
	if err := e.SetFileWorkHost(host, func(id string) (string, int, error) {
		root, err := PrepareWork(base, id, os.Geteuid())
		return root, os.Geteuid(), err
	}); err != nil {
		t.Fatal(err)
	}
	e.ReceiveMessage(p, naturalMessage("first", "", "Prepare a draft"))
	awaitFileCondition(t, func() bool { return shared.turns.Load() == 1 })
	msg := naturalMessage("racing", "", "Use this revised budget", core.FileAttachment{FileName: "budget.xlsx", Data: packageBytes(t, documentParts("xlsx"))})
	ack := make(chan bool, 1)
	msg.OnAccepted = func() {
		s := core.NewSessionManager(path).GetOrCreateActive(msg.SessionKey)
		ack <- len(s.FileTurns) == 1 && s.FileTurns[0].Principal.MessageID == "racing"
	}
	received := make(chan struct{})
	go func() { e.ReceiveMessage(p, msg); close(received) }()
	select {
	case <-host.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("intake not reached")
	}
	nextFileResult(t, shared)
	awaitFileIdle(t, e)
	close(host.release)
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("intake did not finish")
	}
	select {
	case saved := <-ack:
		if !saved {
			t.Fatal("busy supplement acknowledged without recovery record")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supplement not acknowledged")
	}
	if got := nextFileResult(t, shared); len(got.Inputs) != 1 {
		t.Fatal("deferred attachment not activated")
	}
	awaitFileIdle(t, e)
	e.ReceiveMessage(p, naturalMessage("racing", "", "Use this revised budget"))
	if shared.turns.Load() != 2 {
		t.Fatal("accepted supplement replayed")
	}
}

func naturalMessage(id, parent, text string, files ...core.FileAttachment) *core.Message {
	return &core.Message{Platform: "fixture", SessionKey: "fixture:room:staff-a", ChannelID: "room", UserID: "staff-a",
		MessageID: id, ParentMessageID: parent, Content: text, Files: files, ControlledFileWork: true, FileWorkPrivate: true,
		ReplyCtx: fileJourneyRoute{"staff-a", id}}
}

func awaitFileCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("file conversation did not reach expected state")
}

func awaitFileIdle(t *testing.T, e *core.Engine) {
	t.Helper()
	awaitFileCondition(t, func() bool {
		for _, session := range e.GetSessions().ListSessions("fixture:room:staff-a") {
			if session.Busy() {
				return false
			}
		}
		return true
	})
}

func nextFileResult(t *testing.T, shared *fileJourneyShared) core.FileWorkContext {
	t.Helper()
	shared.steps <- fileJourneyStep{}
	select {
	case result := <-shared.results:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.work
	case <-time.After(5 * time.Second):
		t.Fatal("no model fixture result")
	}
	return core.FileWorkContext{}
}

func conversationEngine(t *testing.T, f fixture, p *fileJourneyPlatform, shared *fileJourneyShared, base, path string) *core.Engine {
	t.Helper()
	e := core.NewEngine("file-project", &fileJourneyAgent{shared: shared}, []core.Platform{p}, path, core.LangEnglish)
	if err := e.SetFileWorkHost(f.host, func(session string) (string, int, error) {
		root, err := PrepareWork(base, session, os.Geteuid())
		return root, os.Geteuid(), err
	}); err != nil {
		t.Fatal(err)
	}
	shared.handler = e.ActionToolHandler()
	t.Cleanup(func() { _ = e.Stop() })
	return e
}

func contextOnlyJourney() *fileJourneyShared {
	s := &fileJourneyShared{steps: make(chan fileJourneyStep, 4), results: make(chan fileJourneyResult, 4)}
	s.external = func(a *fileJourneyAgent, _ fileJourneyStep) (r fileJourneyResult) {
		r.err = a.tool("work-context", map[string]any{}, &r.work)
		return
	}
	return s
}

func TestCUJ_FilePrivateConversation_SupplementTextReplyAndNewWork(t *testing.T) {
	p := &fileJourneyPlatform{}
	f := newFixture(t, p.SendFileWithReceipt)
	base := t.TempDir()
	_ = os.Chmod(base, 0700)
	shared := contextOnlyJourney()
	e := conversationEngine(t, f, p, shared, base, filepath.Join(t.TempDir(), "sessions.json"))
	e.ReceiveMessage(p, naturalMessage("proposal", "", "Prepare the fictional proposal", core.FileAttachment{FileName: "brief.docx", Data: packageBytes(t, documentParts("docx"))}))
	awaitFileCondition(t, func() bool { return shared.turns.Load() == 1 })
	supplement := naturalMessage("supplement", "", "Reduce the budget", core.FileAttachment{FileName: "budget.xlsx", Data: packageBytes(t, documentParts("xlsx"))})
	e.ReceiveMessage(p, supplement)
	_, visible := p.snapshot()
	if !strings.Contains(strings.Join(visible, " "), "Supplement saved") || shared.turns.Load() != 1 {
		t.Fatal("busy supplement not acknowledged or sent mid-turn")
	}
	e.ReceiveMessage(p, naturalMessage("supplement", "", "Reduce the budget"))
	first := nextFileResult(t, shared)
	if len(first.Inputs) != 1 {
		t.Fatal("busy supplement became visible in the current turn")
	}
	second := nextFileResult(t, shared)
	awaitFileIdle(t, e)
	if first.WorkID != second.WorkID || len(second.Inputs) != 2 {
		t.Fatal("supplement lost original work or attachment")
	}
	e.ReceiveMessage(p, naturalMessage("supplement", "", "Reduce the budget"))
	if shared.turns.Load() != 2 {
		t.Fatal("completed supplement replayed")
	}
	e.ReceiveMessage(p, naturalMessage("other", "", "换个事，做执行清单"))
	other := nextFileResult(t, shared)
	awaitFileIdle(t, e)
	if other.WorkID == first.WorkID {
		t.Fatal("explicit new work reused old work")
	}
	e.ReceiveMessage(p, naturalMessage("back", "text-proposal", "Revise that proposal again"))
	back := nextFileResult(t, shared)
	awaitFileIdle(t, e)
	if back.WorkID != first.WorkID {
		t.Fatal("reply to bot text did not return to original work")
	}
	_, visible = p.snapshot()
	if !strings.Contains(strings.Join(visible, " "), "FILE-CUJ-DONE:back") {
		t.Fatal("revised answer was not returned")
	}
}

func TestCUJ_FileSupplements_RestartWaitsForContinueWithoutReplay(t *testing.T) {
	for _, crashState := range []string{"queued", "started"} {
		t.Run(crashState, func(t *testing.T) {
			p := &fileJourneyPlatform{}
			f := newFixture(t, p.SendFileWithReceipt)
			base := t.TempDir()
			_ = os.Chmod(base, 0700)
			path := filepath.Join(t.TempDir(), "sessions.json")
			shared := contextOnlyJourney()
			e := conversationEngine(t, f, p, shared, base, path)
			e.ReceiveMessage(p, naturalMessage("original", "", "Prepare the fictional plan"))
			awaitFileCondition(t, func() bool { return shared.turns.Load() == 1 })
			e.ReceiveMessage(p, naturalMessage("pending", "", "Use 22 tables instead", core.FileAttachment{FileName: "revised.xlsx", Data: packageBytes(t, documentParts("xlsx"))}))
			if crashState == "started" {
				nextFileResult(t, shared)
				awaitFileCondition(t, func() bool { return shared.turns.Load() == 2 })
			}
			if err := e.Stop(); err != nil {
				t.Fatal(err)
			}
			awaitFileIdle(t, e)
			before := shared.turns.Load()
			restored := conversationEngine(t, f, p, shared, base, path)
			restored.OnPlatformReady(p)
			_, visible := p.snapshot()
			if !strings.Contains(strings.Join(visible, " "), "Unfinished supplements") || shared.turns.Load() != before {
				t.Fatal("restart lost supplement or automatically ran model")
			}
			restored.ReceiveMessage(p, naturalMessage("resume", "", "continue"))
			resumed := nextFileResult(t, shared)
			if len(resumed.Inputs) != 1 {
				t.Fatal("restart lost saved attachment")
			}
			awaitFileIdle(t, restored)
			_, visible = p.snapshot()
			if !strings.Contains(strings.Join(visible, " "), "FILE-CUJ-DONE:resume") || shared.turns.Load() != before+1 {
				t.Fatal("continue replayed old turns or produced no answer")
			}
			restored.ReceiveMessage(p, naturalMessage("resume", "", "continue"))
			if shared.turns.Load() != before+1 {
				t.Fatal("redelivered recovery request ran another turn")
			}
		})
	}
}
