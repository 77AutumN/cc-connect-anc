package files

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestCaseArtifactImportUsesConfirmedSnapshotAndOwnWorkspace(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if err := f.host.SetGroupReferences(hash([]byte("fictional-group"))); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	b := f.binding
	b.Group, b.SessionID, b.Principal.MessageID = true, "group-source", "source-message"
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
	data := output(t, f, "xlsx")
	if _, err = deliver(t, f, "result.xlsx", data, 0); err != nil {
		t.Fatal(err)
	}
	target := b
	target.SessionID = "operations-work"
	target.Principal.UserID, target.Principal.SessionKey, target.Principal.Project, target.Principal.MessageID = "operations", "operations-session", "operations-project", "operations-message"
	target.WorkRoot, err = PrepareWork(base, target.SessionID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	work, err := f.host.Bind(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result.xlsx"), []byte("model replaced its output"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"receipt":"receipt-fictional"}`), target.Principal, work.WorkID)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		var imported WorkContext
		if err := json.Unmarshal(encoded, &imported); err != nil {
			t.Fatal(err)
		}
		if len(imported.Inputs) != 1 || imported.Inputs[0].Source.WorkID != f.context.WorkID || filepath.Dir(imported.Inputs[0].Path) != filepath.Join(target.WorkRoot, "inputs") {
			t.Fatal(imported)
		}
		actual, err := os.ReadFile(imported.Inputs[0].Path)
		if err != nil || hash(actual) != hash(data) {
			t.Fatal("not the preserved snapshot", err)
		}
	}
	if _, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"receipt":"receipt-fictional","path":"other-account"}`), target.Principal, work.WorkID); err == nil {
		t.Fatal("model path accepted")
	}
	foreign := target.Principal
	foreign.ChatID = "different-group"
	if _, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"receipt":"receipt-fictional"}`), foreign, work.WorkID); err == nil {
		t.Fatal("foreign chat accepted")
	}
	if err := f.store.change(ctx, func(st *state) error {
		for _, d := range st.Works[f.context.WorkID].Deliveries {
			d.Status = "unknown"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Tool(ctx, "host-case-import", json.RawMessage(`{"receipt":"receipt-fictional"}`), target.Principal, work.WorkID); err == nil {
		t.Fatal("unknown outcome imported")
	}
}

func TestProjectArtifactPrivateImportAndExplicitReferenceUseSameGuard(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	realm := hash([]byte("fictional-project-group"))
	if err := f.host.SetGroupReferences(realm); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	source := f.binding
	source.Group, source.SessionID, source.Principal.MessageID = true, "project-source", "source-message"
	var err error
	source.WorkRoot, err = PrepareWork(base, source.SessionID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	f.binding = source
	f.context, err = f.host.Bind(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	data := output(t, f, "xlsx")
	if _, err := deliver(t, f, "result.xlsx", data, 0); err != nil {
		t.Fatal(err)
	}
	target := source
	target.Group, target.SessionID = false, "private-project-work"
	target.Principal.UserID, target.Principal.ChatID = "operations", "registered-private"
	target.Principal.Project, target.Principal.SessionKey, target.Principal.MessageID = "operations-private", "private-session", "private-message"
	target.WorkRoot, err = PrepareWork(base, target.SessionID, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	work, err := f.host.Bind(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"receipt":"receipt-fictional"}`)
	// Legacy group access alone must never cross into private conversations.
	if _, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID); err == nil {
		t.Fatal("private import enabled without project authorization")
	}
	granted := false
	f.host.SetProjectAccess(func(_ context.Context, p core.ActionPrincipal, id, receipt string, requireBinding bool) (string, string, error) {
		if !granted || p != target.Principal || id != work.WorkID || receipt != "receipt-fictional" || !requireBinding {
			return "", "", ErrScope
		}
		return source.Principal.ChatID, realm, nil
	})
	if _, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID); err == nil {
		t.Fatal("missing membership or project binding accepted")
	}
	sameGroup := target.Principal
	sameGroup.ChatID = source.Principal.ChatID
	if _, err := f.host.FindByMessage(ctx, sameGroup, "receipt-fictional"); err == nil {
		t.Fatal("explicit receipt bypassed project guard")
	}
	granted = true
	result, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var imported WorkContext
	if err := json.Unmarshal(encoded, &imported); err != nil || len(imported.Inputs) != 1 {
		t.Fatal(result, err)
	}
	actual, err := os.ReadFile(imported.Inputs[0].Path)
	if err != nil || hash(actual) != hash(data) || filepath.Dir(imported.Inputs[0].Path) != filepath.Join(target.WorkRoot, "inputs") {
		t.Fatal("private input is not the authorized snapshot", err)
	}
	// Even a project grant cannot turn an unknown transport outcome into proof.
	if err := f.store.change(ctx, func(st *state) error {
		for _, d := range st.Works[f.context.WorkID].Deliveries {
			d.Status = "unknown"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Tool(ctx, "host-case-import", input, target.Principal, work.WorkID); err == nil {
		t.Fatal("unknown delivery reimported")
	}
}
