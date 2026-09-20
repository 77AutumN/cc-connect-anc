package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func documentParts(ext string) map[string]string {
	part, content, body := "word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml", `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Fictional reception brief</w:t></w:r></w:p></w:body></w:document>`
	if ext == "xlsx" {
		part, content, body = "xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets/></workbook>`
	}
	return map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/` + part + `" ContentType="` + content + `"/></Types>`,
		"_rels/.rels":         `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="r1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="` + part + `"/></Relationships>`,
		part:                  body,
	}
}

func packageBytes(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range parts {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestOOXMLRejectsDisguisedActiveAndMalformedFiles(t *testing.T) {
	for _, ext := range []string{"docx", "xlsx"} {
		if err := validateOOXML("sample."+ext, packageBytes(t, documentParts(ext))); err != nil {
			t.Fatal(ext, err)
		}
	}
	ordinary := documentParts("xlsx")
	ordinary["xl/worksheets/sheet1.xml"] = `<worksheet><f>SUM(A1:A3)</f><f>SUM(Table1[Sales])</f></worksheet>`
	if err := validateOOXML("normal.xlsx", packageBytes(t, ordinary)); err != nil {
		t.Fatal("ordinary formulas rejected", err)
	}
	for _, name := range []string{"sample.xlsm", "sample.zip", "sample.docx"} {
		if validateOOXML(name, []byte("not a document")) == nil {
			t.Fatal("accepted disguised input", name)
		}
	}
	cases := map[string]func(map[string]string){
		"macro": func(p map[string]string) { p["word/vbaProject.bin"] = "macro" },
		"external": func(p map[string]string) {
			p["word/_rels/document.xml.rels"] = `<Relationships><Relationship TargetMode="External" Target="https://example.invalid/file"/></Relationships>`
		},
		"embedded":         func(p map[string]string) { p["word/embeddings/object.bin"] = "object" },
		"traversal":        func(p map[string]string) { p["../outside.xml"] = `<x/>` },
		"alias":            func(p map[string]string) { p["WORD/DOCUMENT.XML"] = `<x/>` },
		"doctype":          func(p map[string]string) { p["word/document.xml"] = `<!DOCTYPE x SYSTEM "file:///secret"><x/>` },
		"missing_type":     func(p map[string]string) { delete(p, "[Content_Types].xml") },
		"missing_relation": func(p map[string]string) { delete(p, "_rels/.rels") },
		"wrong_root":       func(p map[string]string) { p["word/document.xml"] = `<workbook/>` },
		"malformed":        func(p map[string]string) { p["word/document.xml"] = `<unclosed>` },
		"formula": func(p map[string]string) {
			p["xl/worksheets/sheet1.xml"] = `<worksheet><f>WEBSERVICE("https://example.invalid")</f></worksheet>`
		},
		"field": func(p map[string]string) {
			p["word/header1.xml"] = `<header><instrText>DDEAUTO command</instrText></header>`
		},
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			p := documentParts("docx")
			modify(p)
			if validateOOXML("sample.docx", packageBytes(t, p)) == nil {
				t.Fatal("unsafe package accepted")
			}
		})
	}
	if validateOOXML("sample.xlsx", packageBytes(t, documentParts("docx"))) == nil {
		t.Fatal("extension/content mismatch accepted")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestOOXMLBudgets(t *testing.T) {
	if validateOOXML("large.docx", make([]byte, MaxFileBytes+1)) == nil {
		t.Fatal("oversize accepted")
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("word/document.xml")
	if _, err := io.CopyN(f, zeros{}, maxExpandedBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if validateOOXML("bomb.docx", b.Bytes()) == nil {
		t.Fatal("expansion bomb accepted")
	}
}

func TestStrictToolInputRejectsOverridesAndAmbiguity(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{"work_id":"a","work_id":"b"}`, `{"recipient":"x"}`, `{"principal":{}}`, `{"path":"x","expected_version":0.5}`, `{"path":"x","expected_version":"0"}`} {
		var r deliverRequest
		if strictInput(json.RawMessage(raw), &r) == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, path := range []string{"/etc/secret.docx", "../other.docx", "a/../b.docx", `a\b.docx`, "a//b.docx", "C:/file.docx", "./file.docx"} {
		if relativeFile(path) {
			t.Fatal("unsafe path accepted", path)
		}
	}
	if !relativeFile("draft/reception.docx") {
		t.Fatal("safe relative file rejected")
	}
}

func TestMissingOrCorruptStoreNeverCreatesReplacement(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	missing := filepath.Join(dir, "missing.sqlite")
	if _, err := Open(missing); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("Open created missing store")
	}
	broken := filepath.Join(dir, "broken.sqlite")
	if err := os.WriteFile(broken, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(broken); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(broken); string(b) != "corrupt" {
		t.Fatal("corrupt evidence overwritten")
	}
}

type fixture struct {
	host      *Host
	store     *Store
	binding   Binding
	context   WorkContext
	database  string
	snapshots string
}

func newFixture(t *testing.T, sender Sender) fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership/no-follow boundary; Linux CI runs the full host")
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state", "snapshots", "work", "work/inputs", "work/outputs"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(base, "state", "files.sqlite")
	if err := Initialize(db); err != nil {
		t.Fatal(err)
	}
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if sender == nil {
		sender = func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
			return "receipt-fictional", nil
		}
	}
	h, err := New(s, filepath.Join(base, "snapshots"), sender)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	binding := Binding{Principal: core.ActionPrincipal{Platform: "fixture", UserID: "staff-a", ChatID: "chat-a", SessionKey: "session-a", Project: "project-a", MessageID: "message-a"}, SessionID: "native-a", Route: json.RawMessage(`{"audience":"original-thread"}`), WorkRoot: filepath.Join(base, "work"), OwnerUID: os.Geteuid()}
	w, err := h.Bind(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{host: h, store: s, binding: binding, context: w, database: db, snapshots: filepath.Join(base, "snapshots")}
}

func output(t *testing.T, f fixture, ext string) []byte {
	t.Helper()
	data := packageBytes(t, documentParts(ext))
	if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result."+ext), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func deliver(t *testing.T, f fixture, name string, data []byte, version int) (map[string]any, error) {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"work_id": f.context.WorkID, "path": name, "sha256": hash(data), "expected_version": version})
	return f.host.Tool(context.Background(), "file-deliver", input, f.binding.Principal, f.context.WorkID)
}

func TestWorkBindingInputsAndFrozenRoute(t *testing.T) {
	var sentRoute json.RawMessage
	f := newFixture(t, func(_ context.Context, route json.RawMessage, file core.FileAttachment, id string) (string, error) {
		sentRoute = append([]byte{}, route...)
		if uuidErr := validateOOXML(file.FileName, file.Data); uuidErr != nil || id == "" {
			t.Fatal("invalid send")
		}
		return "result-message", nil
	})
	b := f.binding
	b.Principal.MessageID = "message-b"
	b.Route = json.RawMessage(`{"audience":"later-speaker"}`)
	data := packageBytes(t, documentParts("xlsx"))
	b.Inputs = []core.FileAttachment{{FileName: "fictional.xlsx", Data: data}}
	w, err := f.host.Bind(context.Background(), b)
	if err != nil || w.WorkID != f.context.WorkID || len(w.Inputs) != 1 {
		t.Fatal(w, err)
	}
	read, err := os.ReadFile(w.Inputs[0].Path)
	if err != nil || !bytes.Equal(read, data) || w.Inputs[0].SHA256 != hash(data) {
		t.Fatal("original missing", err)
	}
	if again, err := f.host.Bind(context.Background(), b); err != nil || len(again.Inputs) != 1 {
		t.Fatal("duplicate input", err)
	}
	generated := output(t, f, "docx")
	result, err := deliver(t, f, "result.docx", generated, 0)
	if err != nil || result["status"] != "accepted" {
		t.Fatal(result, err)
	}
	if string(sentRoute) != string(f.binding.Route) {
		t.Fatal("recipient followed later message")
	}
	for _, id := range []string{"message-a", "message-b", "result-message"} {
		ref, err := f.host.FindByMessage(context.Background(), b.Principal, id)
		if err != nil || ref.WorkID != w.WorkID || ref.SessionID != b.SessionID {
			t.Fatal(ref, err)
		}
	}
	bad := b
	bad.SessionID = "native-b"
	if _, err := f.host.Bind(context.Background(), bad); !errors.Is(err, ErrScope) {
		t.Fatal("reused work root", err)
	}
	bad = b
	bad.Principal.MessageID = ""
	if _, err := f.host.Bind(context.Background(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty origin accepted", err)
	}
}

func TestDeliveryDeduplicatesConcurrentRequestsAndVersions(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
		calls.Add(1)
		close(entered)
		<-release
		return "receipt-a", nil
	})
	data := output(t, f, "docx")
	finished := make(chan error, 1)
	go func() { _, err := deliver(t, f, "result.docx", data, 0); finished <- err }()
	<-entered
	result, err := deliver(t, f, "result.docx", data, 0)
	if err != nil || result["status"] != "submitted" {
		t.Fatal(result, err)
	}
	if _, err := deliver(t, f, "result.docx", data, 1); !errors.Is(err, ErrUncertain) {
		t.Fatal("inflight resend allowed", err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	again, err := deliver(t, f, "result.docx", data, 0)
	if err != nil || again["status"] != "accepted" || again["delivery_id"] != result["delivery_id"] || calls.Load() != 1 {
		t.Fatal(again, err, calls.Load())
	}
	f.host.send = func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
		return "receipt-b", nil
	}
	if revised, err := deliver(t, f, "result.docx", data, 1); err != nil || revised["version"] != float64(2) {
		t.Fatal(revised, err)
	}
}

func TestDeliveryUnknownFailedAndRestartNeverResend(t *testing.T) {
	for _, test := range []struct {
		name            string
		err             error
		receipt, status string
	}{{"network", errors.New("timeout"), "", "unknown"}, {"empty", nil, "", "unknown"}, {"rejected", core.ErrFileNotSubmitted, "", "failed"}} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			f := newFixture(t, func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
				calls++
				return test.receipt, test.err
			})
			data := output(t, f, "xlsx")
			result, err := deliver(t, f, "result.xlsx", data, 0)
			if err != nil || result["status"] != test.status {
				t.Fatal(result, err)
			}
			if _, err := deliver(t, f, "result.xlsx", data, 0); err != nil || calls != 1 {
				t.Fatal("replayed send", err)
			}
			if test.status == "unknown" {
				if _, err := deliver(t, f, "result.xlsx", data, 1); !errors.Is(err, ErrUncertain) {
					t.Fatal(err)
				}
			}
		})
	}
	f := newFixture(t, nil)
	data := output(t, f, "docx")
	result, err := deliver(t, f, "result.docx", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.change(context.Background(), func(st *state) error {
		d := st.Works[f.context.WorkID].Deliveries[result["delivery_id"].(string)]
		d.Status = "submitted"
		d.MessageReceipt = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.store.Close()
	reopened, err := Open(f.database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	f.host.store = reopened
	result, err = deliver(t, f, "result.docx", data, 0)
	if err != nil || result["status"] != "unknown" {
		t.Fatal(result, err)
	}
}

func TestScopePathsLinksAndDigestCannotEscapeWork(t *testing.T) {
	f := newFixture(t, nil)
	data := output(t, f, "docx")
	for _, field := range []string{"platform", "user", "chat", "session", "project"} {
		p := f.binding.Principal
		switch field {
		case "platform":
			p.Platform = "other"
		case "user":
			p.UserID = "other"
		case "chat":
			p.ChatID = "other"
		case "session":
			p.SessionKey = "other"
		case "project":
			p.Project = "other"
		}
		if _, err := f.host.Tool(context.Background(), "work-context", json.RawMessage(`{}`), p, f.context.WorkID); !errors.Is(err, ErrScope) {
			t.Fatal(field, err)
		}
		if _, err := f.host.FindByMessage(context.Background(), p, "message-a"); !errors.Is(err, ErrScope) {
			t.Fatal(field, err)
		}
	}
	for name, target := range map[string]string{"symbolic.docx": "result.docx", "outside.docx": filepath.Join(f.binding.WorkRoot, "inputs", "unknown.docx")} {
		if err := os.Symlink(target, filepath.Join(f.context.OutputDir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := deliver(t, f, name, data, 0); err == nil {
			t.Fatal("symlink accepted")
		}
	}
	if err := os.Link(filepath.Join(f.context.OutputDir, "result.docx"), filepath.Join(f.context.OutputDir, "hard.docx")); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(t, f, "hard.docx", data, 0); err == nil {
		t.Fatal("hardlink accepted")
	}
	if err := os.Remove(filepath.Join(f.context.OutputDir, "hard.docx")); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(t, f, "result.docx", []byte("different"), 0); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	if err := f.store.change(context.Background(), func(st *state) error { st.Works[f.context.WorkID].OwnerUID++; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver(t, f, "result.docx", data, 0); err == nil {
		t.Fatal("foreign owner accepted")
	}
}

func TestSnapshotsAndDeletedStoreRemainTruthful(t *testing.T) {
	var f fixture
	f = newFixture(t, func(_ context.Context, _ json.RawMessage, file core.FileAttachment, _ string) (string, error) {
		if err := os.WriteFile(filepath.Join(f.context.OutputDir, "result.docx"), []byte("replaced"), 0600); err != nil {
			t.Fatal(err)
		}
		if validateOOXML(file.FileName, file.Data) != nil {
			t.Fatal("sender reread mutable path")
		}
		return "frozen-receipt", nil
	})
	data := output(t, f, "docx")
	result, err := deliver(t, f, "result.docx", data, 0)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(filepath.Join(f.snapshots, result["delivery_id"].(string)+".docx"))
	if err != nil || !bytes.Equal(snapshot, data) {
		t.Fatal("snapshot changed", err)
	}
	if err := os.Remove(f.database); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Tool(context.Background(), "work-context", json.RawMessage(`{}`), f.binding.Principal, f.context.WorkID); !errors.Is(err, ErrUnavailable) {
		t.Fatal("lost store accepted", err)
	}
	if _, err := os.Stat(f.database); !os.IsNotExist(err) {
		t.Fatal("store silently recreated")
	}
}

func TestInputBatchFailsBeforeSavingAnyPart(t *testing.T) {
	f := newFixture(t, nil)
	b := f.binding
	b.Principal.MessageID = "message-new"
	b.Inputs = []core.FileAttachment{{FileName: "good.docx", Data: packageBytes(t, documentParts("docx"))}, {FileName: "bad.xlsx", Data: []byte("invalid")}}
	if _, err := f.host.Bind(context.Background(), b); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(b.WorkRoot, "inputs"))
	if err != nil || len(entries) != 0 {
		t.Fatal("partial input published", err)
	}
	contextResult, err := f.host.Tool(context.Background(), "work-context", json.RawMessage(`{}`), b.Principal, f.context.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(contextResult)
	if strings.Contains(string(encoded), "audience") || strings.Contains(string(encoded), "owner_uid") || strings.Contains(string(encoded), "WorkRoot") {
		t.Fatal("host fields leaked")
	}
}

func TestPrepareWorkNeverRepairsExistingOrFollowsLinks(t *testing.T) {
	f := newFixture(t, nil)
	base := filepath.Dir(f.binding.WorkRoot)
	path, err := PrepareWork(base, "new-native-session", os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	if again, err := PrepareWork(base, "new-native-session", os.Geteuid()); err != nil || again != path {
		t.Fatal(again, err)
	}
	if err := os.Chmod(filepath.Join(path, "outputs"), 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWork(base, "new-native-session", os.Geteuid()); err == nil {
		t.Fatal("existing mode was silently repaired")
	}
	if info, _ := os.Stat(filepath.Join(path, "outputs")); info.Mode().Perm() != 0777 {
		t.Fatal("changed existing mode")
	}
	link := filepath.Join(base, hash([]byte("linked-session")))
	if err := os.Symlink(f.binding.WorkRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWork(base, "linked-session", os.Geteuid()); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := f.host.FindByMessage(context.Background(), f.binding.Principal, "not-seen"); !errors.Is(err, core.ErrFileWorkNotFound) {
		t.Fatal(err)
	}
}
