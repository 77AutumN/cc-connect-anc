package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCUJ_GroupFile_ExplicitArtifactHandoffKeepsActorsAndOtherWorkSeparate(t *testing.T) {
	pA, pB := &fileJourneyPlatform{}, &fileJourneyPlatform{}
	f := newFixture(t, pA.SendFileWithReceipt)
	if err := f.host.SetGroupReferences(hash([]byte("fictional-bot-and-room"))); err != nil {
		t.Fatal(err)
	}
	hB, err := New(f.store, f.snapshots, pB.SendFileWithReceipt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hB.Close() })
	if err := hB.SetGroupReferences(f.host.groupRealm); err != nil {
		t.Fatal(err)
	}
	fB := f
	fB.host = hB
	newShared := func() *fileJourneyShared {
		return &fileJourneyShared{steps: make(chan fileJourneyStep, 1), results: make(chan fileJourneyResult, 1)}
	}
	sA, sB := newShared(), newShared()
	baseA, baseB := t.TempDir(), t.TempDir()
	_ = os.Chmod(baseA, 0700)
	_ = os.Chmod(baseB, 0700)
	eA := conversationEngine(t, f, pA, sA, baseA, filepath.Join(t.TempDir(), "a.json"))
	eB := conversationEngine(t, fB, pB, sB, baseB, filepath.Join(t.TempDir(), "b.json"))
	message := func(id, parent, actor string, input ...core.FileAttachment) *core.Message {
		m := naturalMessage(id, parent, "Revise the selected fictional brief", input...)
		m.UserID, m.SessionKey, m.FileWorkPrivate = actor, "fixture:room:"+actor, false
		m.ReplyCtx = fileJourneyRoute{actor, id}
		return m
	}
	run := func(e *core.Engine, p *fileJourneyPlatform, shared *fileJourneyShared, m *core.Message, name, from, to string) fileJourneyResult {
		t.Helper()
		shared.steps <- fileJourneyStep{name, from, to, "docx", ""}
		e.ReceiveMessage(p, m)
		select {
		case r := <-shared.results:
			if r.err != nil || r.artifact.Status != "accepted" {
				t.Fatal("file was not delivered", r.err)
			}
			awaitFileCondition(t, func() bool {
				for _, s := range e.GetSessions().ListSessions(m.SessionKey) {
					if s.Busy() {
						return false
					}
				}
				return true
			})
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("group revision did not complete")
		}
		return fileJourneyResult{}
	}
	input := core.FileAttachment{FileName: "brief.docx", Data: packageBytes(t, documentParts("docx"))}
	a := run(eA, pA, sA, message("a1", "", "staff-a", input), "brief.docx", "Fictional reception brief", "Draft A")
	b := run(eB, pB, sB, message("b1", a.artifact.MessageReceipt, "staff-b"), a.artifact.Name, "Draft A", "Draft B")
	if a.work.WorkID == b.work.WorkID || len(b.work.Inputs) != 1 || b.work.Inputs[0].Source == nil || b.work.Inputs[0].Source.WorkID != a.work.WorkID || !strings.HasPrefix(b.work.OutputDir, baseB+string(os.PathSeparator)) {
		t.Fatal("handoff did not preserve separate actor work and provenance")
	}
	other := run(eB, pB, sB, message("b-other", "", "staff-b", input), "brief.docx", "Fictional reception brief", "Unrelated draft")
	if other.work.WorkID == b.work.WorkID {
		t.Fatal("unrelated work reused handoff")
	}
	b2 := run(eB, pB, sB, message("b2", a.artifact.MessageReceipt, "staff-b"), a.artifact.Name, "Draft B", "Draft B revised")
	if b2.work.WorkID != b.work.WorkID || b2.artifact.Version != 2 {
		t.Fatal("repeated explicit reference lost the imported work")
	}
	a2 := run(eA, pA, sA, message("a2", b2.artifact.MessageReceipt, "staff-a"), b2.artifact.Name, "Draft B revised", "Final A")
	if a2.work.WorkID == a.work.WorkID || len(a2.work.Inputs) != 1 || a2.work.Inputs[0].Source.WorkID != b.work.WorkID {
		t.Fatal("return handoff shared a private conversation")
	}
	for _, pair := range []struct {
		p     *fileJourneyPlatform
		actor string
	}{{pA, "staff-a"}, {pB, "staff-b"}} {
		files, replies := pair.p.snapshot()
		if len(files) == 0 || !strings.Contains(strings.Join(replies, " "), "FILE-CUJ-DONE") {
			t.Fatal("no visible result")
		}
		for _, sent := range files {
			if sent.route.Recipient != pair.actor {
				t.Fatal("delivery used another actor's route")
			}
		}
	}
	// Another actor's task/text is not a unique artifact; private and cross-room replies stay blocked.
	for _, m := range []*core.Message{message("bad-task", "a1", "staff-b"), message("bad-text", "text-a1", "staff-b"), message("bad-private", a.artifact.MessageReceipt, "staff-b"), message("bad-room", a.artifact.MessageReceipt, "staff-b")} {
		if m.MessageID == "bad-private" {
			m.FileWorkPrivate = true
		}
		if m.MessageID == "bad-room" {
			m.ChannelID = "other-room"
			m.SessionKey = "fixture:other-room:staff-b"
		}
		before := sB.turns.Load()
		eB.ReceiveMessage(pB, m)
		if sB.turns.Load() != before {
			t.Fatal("unrelated reply reached model")
		}
	}
	if err := f.store.change(context.Background(), func(st *state) error {
		if st.Schema != 2 || !validState(*st) {
			t.Fatal("group provenance lost its ledger version")
		}
		old := *st
		old.Schema = 1
		if validState(old) {
			t.Fatal("group provenance accepted without downgrade protection")
		}
		if len(st.Works[a.work.WorkID].Deliveries) != 1 || len(st.Works[b.work.WorkID].Deliveries) != 2 {
			t.Fatal("old versions changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGroupArtifactRejectsUnknownScopeAndDamagedSnapshot(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if err := f.store.change(ctx, func(st *state) error {
		if st.Schema != 1 {
			t.Fatal("default-off private ledger version changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	realm := hash([]byte("fixture-bot-group"))
	if err := f.host.SetGroupReferences(realm); err != nil {
		t.Fatal(err)
	}
	b := f.binding
	b.SessionID, b.Principal.MessageID, b.Group = "group-source", "group-task", true
	base := t.TempDir()
	_ = os.Chmod(base, 0700)
	var err error
	b.WorkRoot, err = PrepareWork(base, b.SessionID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	f.binding = b
	f.context, err = f.host.Bind(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	data := output(t, f, "docx")
	if _, err = deliver(t, f, "result.docx", data, 0); err != nil {
		t.Fatal(err)
	}
	recipient := b.Principal
	recipient.UserID, recipient.SessionKey, recipient.Project, recipient.MessageID = "staff-b", "session-b", "project-b", "revision"
	check := func(p core.ActionPrincipal, receipt string, want error) {
		t.Helper()
		ref, err := f.host.FindByMessage(ctx, p, receipt)
		if want == nil {
			if err != nil || !ref.Import {
				t.Fatal("valid group artifact unavailable", err)
			}
			return
		}
		if !errors.Is(err, want) {
			t.Fatalf("got %v, want %v", err, want)
		}
	}
	check(recipient, "receipt-fictional", nil)
	check(recipient, "group-task", core.ErrFileWorkNeedsArtifact)
	for _, field := range []string{"platform", "chat"} {
		p := recipient
		if field == "platform" {
			p.Platform = "other-platform"
		} else {
			p.ChatID = "other-chat"
		}
		check(p, "receipt-fictional", ErrScope)
	}
	for _, setting := range []string{"", hash([]byte("other-bot"))} {
		f.host.groupRealm = setting
		check(recipient, "receipt-fictional", ErrScope)
	}
	f.host.groupRealm = realm
	var snapshot string
	setStatus := func(status string) {
		t.Helper()
		if err := f.store.change(ctx, func(st *state) error {
			for _, d := range st.Works[f.context.WorkID].Deliveries {
				d.Status = status
				snapshot = d.Snapshot
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"unknown", "submitted"} {
		setStatus(status)
		check(recipient, "receipt-fictional", ErrUncertain)
	}
	setStatus("accepted")
	// Replacing model output must not change the selected accepted snapshot.
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result.docx"), []byte("modified output"), 0600); err != nil {
		t.Fatal(err)
	}
	target := b
	target.Principal, target.SessionID, target.SourceReceipt = recipient, "target", "receipt-fictional"
	target.WorkRoot, err = PrepareWork(base, target.SessionID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	private := target
	private.Group = false
	if _, err := f.host.Bind(ctx, private); !errors.Is(err, ErrScope) {
		t.Fatal("private import accepted", err)
	}
	path := filepath.Join(f.snapshots, snapshot)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged snapshot"), 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Bind(ctx, target); !errors.Is(err, ErrInvalid) {
		t.Fatal("damaged snapshot imported", err)
	}
	if err := os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	work, err := f.host.Bind(ctx, target)
	if err != nil || len(work.Inputs) != 1 {
		t.Fatal("snapshot not imported", err)
	}
	copy, err := os.ReadFile(work.Inputs[0].Path)
	if err != nil || hash(copy) != hash(data) {
		t.Fatal("import read mutable output instead of accepted snapshot")
	}
	if _, err := f.host.Tool(ctx, "work-context", []byte(`{}`), recipient, f.context.WorkID); !errors.Is(err, ErrScope) {
		t.Fatal("recipient gained source work tools")
	}
}
