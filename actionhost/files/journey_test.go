package files

// Real Engine, SessionManager, HTTP tool authentication, file host and SQLite.
// Only agent intentions and platform delivery are fake. This is neither a real
// model eval nor a knowledge-service or Linux launcher acceptance test.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type fileJourneyStep struct{ inputName, from, to, extension, reference string }
type fileJourneyResult struct {
	work     core.FileWorkContext
	artifact core.FileWorkArtifact
	data     []byte
	err      error
}
type fileJourneyShared struct {
	handler  http.Handler
	steps    chan fileJourneyStep
	results  chan fileJourneyResult
	turns    atomic.Int32
	external func(*fileJourneyAgent, fileJourneyStep) fileJourneyResult
}
type fileJourneyAgent struct {
	shared *fileJourneyShared
	root   string
	token  string
	events chan core.Event
	closed atomic.Bool
	done   chan struct{}
}

func (*fileJourneyAgent) Name() string { return "offline-file-fixture" }
func (a *fileJourneyAgent) ForFileWork(root string) (core.Agent, error) {
	return &fileJourneyAgent{shared: a.shared, root: root, events: make(chan core.Event, 4), done: make(chan struct{})}, nil
}
func (a *fileJourneyAgent) SetSessionEnv(env []string) {
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "CC_FILE_ACTION_TOKEN="); ok {
			a.token = value
		}
	}
}
func (a *fileJourneyAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return a, nil
}
func (*fileJourneyAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (*fileJourneyAgent) Stop() error                 { return nil }
func (a *fileJourneyAgent) Events() <-chan core.Event { return a.events }
func (a *fileJourneyAgent) CurrentSessionID() string  { return "fixture-" + hash([]byte(a.root)) }
func (a *fileJourneyAgent) Alive() bool               { return !a.closed.Load() }
func (a *fileJourneyAgent) Close() error {
	if !a.closed.Swap(true) && a.done != nil {
		close(a.done)
	}
	return nil
}
func (*fileJourneyAgent) RespondPermission(string, core.PermissionResult) error {
	return fmt.Errorf("unexpected permission flow")
}

func (a *fileJourneyAgent) tool(command string, input any, result any) error {
	payload, err := json.Marshal(map[string]any{"command": command, "input": input})
	if err != nil {
		return err
	}
	request := httptest.NewRequest(http.MethodPost, "/tool", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+a.token)
	recorder := httptest.NewRecorder()
	a.shared.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		return fmt.Errorf("controlled tool %s returned HTTP %d", command, recorder.Code)
	}
	return json.Unmarshal(recorder.Body.Bytes(), result)
}

func (a *fileJourneyAgent) Send(_ string, messageID string, _ []core.ImageAttachment, files []core.FileAttachment) error {
	a.shared.turns.Add(1)
	var step fileJourneyStep
	select {
	case step = <-a.shared.steps:
	case <-a.done:
		return fmt.Errorf("fixture process stopped")
	}
	result := a.perform(step, files)
	a.shared.results <- result
	a.events <- core.Event{Type: core.EventResult, Content: "FILE-CUJ-DONE:" + messageID, Done: true}
	return nil
}

func (a *fileJourneyAgent) perform(step fileJourneyStep, files []core.FileAttachment) (result fileJourneyResult) {
	if len(files) != 0 {
		result.err = fmt.Errorf("engine forwarded a second attachment copy")
		return
	}
	if a.shared.external != nil {
		return a.shared.external(a, step)
	}
	if result.err = a.tool("work-context", map[string]any{}, &result.work); result.err != nil {
		return
	}
	w := result.work
	if !w.Enabled || w.WorkID == "" || w.OutputDir != filepath.Join(a.root, "outputs") {
		result.err = fmt.Errorf("context does not identify this work")
		return
	}
	var source, reference []byte
	for _, input := range w.Inputs {
		data, err := os.ReadFile(input.Path)
		if err != nil || hash(data) != input.SHA256 {
			result.err = fmt.Errorf("registered original unavailable")
			return
		}
		if input.Name == step.inputName {
			source = data
		}
		if input.Name == step.reference {
			reference = data
		}
	}
	if len(source) == 0 {
		result.err = fmt.Errorf("input not present in work context")
		return
	}
	if len(w.Artifacts) > 0 {
		latest := w.Artifacts[len(w.Artifacts)-1]
		if !relativeFile(latest.Path) || !validHash(latest.SHA256) {
			result.err = fmt.Errorf("prior artifact has invalid path or digest")
			return
		}
		var err error
		source, err = os.ReadFile(filepath.Join(w.OutputDir, latest.Path))
		if err != nil {
			result.err = err
			return
		}
		if hash(source) != latest.SHA256 {
			result.err = fmt.Errorf("prior artifact digest changed")
			return
		}
	}
	parts, err := fileJourneyParts(source)
	if err != nil {
		result.err = err
		return
	}
	part := "xl/worksheets/sheet1.xml"
	if step.extension == "docx" {
		part = "word/document.xml"
	}
	if !strings.Contains(parts[part], step.from) {
		result.err = fmt.Errorf("document data did not match requested revision")
		return
	}
	parts[part] = strings.ReplaceAll(parts[part], step.from, step.to)
	if step.reference != "" {
		approved, err := fileJourneyParts(reference)
		if err != nil || !strings.Contains(approved["word/document.xml"], "Fictional approved venue: Cedar Hall") {
			result.err = fmt.Errorf("selected approved reference was not read")
			return
		}
		parts[part] = strings.Replace(parts[part], "</w:body>", "<w:p><w:r><w:t>Fictional approved venue: Cedar Hall</w:t></w:r></w:p></w:body>", 1)
	}
	result.data, err = fileJourneyPackage(parts)
	if err != nil {
		result.err = err
		return
	}
	name := fmt.Sprintf("revisions/result-v%d.%s", w.LatestVersion+1, step.extension)
	if result.err = os.MkdirAll(filepath.Join(w.OutputDir, "revisions"), 0700); result.err != nil {
		return
	}
	if result.err = os.WriteFile(filepath.Join(w.OutputDir, name), result.data, 0600); result.err != nil {
		return
	}
	result.err = a.tool("file-deliver", map[string]any{"work_id": w.WorkID, "path": name, "sha256": hash(result.data), "expected_version": w.LatestVersion}, &result.artifact)
	return
}

func fileJourneyParts(data []byte) (map[string]string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	parts := map[string]string{}
	for _, file := range r.File {
		stream, err := file.Open()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			return nil, err
		}
		parts[file.Name] = string(body)
	}
	return parts, nil
}

func fileJourneyPackage(parts map[string]string) ([]byte, error) {
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		f, err := w.Create(key)
		if err != nil {
			return nil, err
		}
		if _, err = io.WriteString(f, parts[key]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

type fileJourneyRoute struct {
	Recipient string `json:"recipient"`
	Thread    string `json:"thread"`
}
type fileJourneyDelivery struct {
	route   fileJourneyRoute
	file    core.FileAttachment
	receipt string
}
type fileJourneyPlatform struct {
	mu         sync.Mutex
	deliveries []fileJourneyDelivery
	replies    []string
	authorize  func(core.Message, string) bool
	observe    func(core.Message, string) error
}

func (*fileJourneyPlatform) Name() string { return "fixture" }
func (p *fileJourneyPlatform) SetFileWorkReplyObserver(observe func(core.Message, string) error) {
	p.mu.Lock()
	p.observe = observe
	p.mu.Unlock()
}
func (*fileJourneyPlatform) FileWorkReplyContext(raw json.RawMessage) (any, error) {
	var route fileJourneyRoute
	err := json.Unmarshal(raw, &route)
	return route, err
}
func (*fileJourneyPlatform) Start(core.MessageHandler) error { return nil }
func (*fileJourneyPlatform) Stop() error                     { return nil }
func (p *fileJourneyPlatform) SetFileWorkEnabled(enabled bool, authorize func(core.Message, string) bool) error {
	if !enabled {
		return fmt.Errorf("file capability unexpectedly disabled")
	}
	p.authorize = authorize
	return nil
}
func (*fileJourneyPlatform) FileReplyRoute(reply any) (json.RawMessage, error) {
	return json.Marshal(reply)
}
func (p *fileJourneyPlatform) SendFileWithReceipt(_ context.Context, raw json.RawMessage, file core.FileAttachment, id string) (string, error) {
	var route fileJourneyRoute
	if err := json.Unmarshal(raw, &route); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	receipt := "fixture-receipt-" + id
	p.deliveries = append(p.deliveries, fileJourneyDelivery{route, file, receipt})
	return receipt, nil
}
func (p *fileJourneyPlatform) Reply(_ context.Context, target any, text string) error {
	p.mu.Lock()
	p.replies = append(p.replies, text)
	observe := p.observe
	p.mu.Unlock()
	if route, ok := target.(fileJourneyRoute); ok && observe != nil {
		_ = observe(core.Message{Platform: "fixture", UserID: route.Recipient, ChannelID: "room", SessionKey: "fixture:room:" + route.Recipient, MessageID: route.Thread}, "text-"+route.Thread)
	}
	return nil
}
func (p *fileJourneyPlatform) Send(ctx context.Context, target any, text string) error {
	return p.Reply(ctx, target, text)
}
func (p *fileJourneyPlatform) snapshot() ([]fileJourneyDelivery, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]fileJourneyDelivery(nil), p.deliveries...), append([]string(nil), p.replies...)
}

func TestCUJ_FileWork_OriginalReceiptRevisionNewCustomerAndSelectedReference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX host ownership boundary; Linux CI runs the journey")
	}
	runFileJourney(t, "", os.Geteuid(), nil)
}

func runFileJourney(t *testing.T, base string, modelUID int, configure func(*fileJourneyShared, fixture, *core.Engine)) {
	t.Helper()
	platform := &fileJourneyPlatform{}
	fixture := newFixture(t, platform.SendFileWithReceipt)
	if base == "" {
		base = filepath.Join(t.TempDir(), "works")
		if err := os.Mkdir(base, 0700); err != nil {
			t.Fatal(err)
		}
	}
	shared := &fileJourneyShared{steps: make(chan fileJourneyStep, 1), results: make(chan fileJourneyResult, 1)}
	engine := core.NewEngine("file-project", &fileJourneyAgent{shared: shared}, []core.Platform{platform}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	t.Cleanup(func() { _ = engine.Stop() })
	if err := engine.SetFileWorkHost(fixture.host, func(session string) (string, int, error) {
		root, err := PrepareWork(base, session, modelUID)
		return root, modelUID, err
	}); err != nil {
		t.Fatal(err)
	}
	shared.handler = engine.ActionToolHandler()
	if configure != nil {
		configure(shared, fixture, engine)
	}
	makeMessage := func(id, parent, user string, attachments ...core.FileAttachment) *core.Message {
		return &core.Message{Platform: "fixture", SessionKey: "fixture:room:" + user, ChannelID: "room", UserID: user, MessageID: id, ParentMessageID: parent, ControlledFileWork: true, Content: "Edit the selected fictional document", Files: attachments, ReplyCtx: fileJourneyRoute{user, id}}
	}
	waitIdle := func(key string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			busy := false
			for _, session := range engine.GetSessions().ListSessions(key) {
				busy = busy || session.Busy()
			}
			if !busy {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("engine did not finish file turn")
	}
	run := func(msg *core.Message, step fileJourneyStep) fileJourneyResult {
		t.Helper()
		shared.steps <- step
		engine.ReceiveMessage(platform, msg)
		select {
		case result := <-shared.results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			waitIdle(msg.SessionKey)
			if result.artifact.Status != "accepted" || result.artifact.MessageReceipt == "" {
				t.Fatal("recipient delivery has no accepted receipt")
			}
			return result
		case <-time.After(30 * time.Second):
			t.Fatal("file journey was not processed")
		}
		return fileJourneyResult{}
	}
	spreadsheet := documentParts("xlsx")
	spreadsheet["xl/worksheets/sheet1.xml"] = `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Guest Cedar: 2</t></is></c></row></sheetData></worksheet>`
	spreadsheet["xl/workbook.xml"] = `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Guests" sheetId="1" r:id="s1"/></sheets></workbook>`
	spreadsheet["xl/_rels/workbook.xml.rels"] = `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="s1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`
	spreadsheet["[Content_Types].xml"] = strings.Replace(spreadsheet["[Content_Types].xml"], "</Types>", `<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`, 1)
	original := packageBytes(t, spreadsheet)
	first := run(makeMessage("input-one", "", "staff-a", core.FileAttachment{FileName: "guests.xlsx", Data: original}), fileJourneyStep{"guests.xlsx", "Guest Cedar: 2", "Guest Cedar: 3", "xlsx", ""})
	revised := run(makeMessage("revision-one", "input-one", "staff-a"), fileJourneyStep{"guests.xlsx", "Guest Cedar: 3", "Guest Cedar: 4", "xlsx", ""})
	third := run(makeMessage("revision-two", revised.artifact.MessageReceipt, "staff-a"), fileJourneyStep{"guests.xlsx", "Guest Cedar: 4", "Guest Cedar: 5", "xlsx", ""})
	if first.work.WorkID != revised.work.WorkID || revised.work.WorkID != third.work.WorkID || third.artifact.Version != 3 {
		t.Fatal("replies did not continue the original work")
	}
	next := run(makeMessage("customer-two", "", "staff-a", core.FileAttachment{FileName: "guests.xlsx", Data: original}), fileJourneyStep{"guests.xlsx", "Guest Cedar: 2", "Guest Cedar: 8", "xlsx", ""})
	if next.work.WorkID == first.work.WorkID || next.work.OutputDir == first.work.OutputDir || next.artifact.Version != 1 {
		t.Fatal("new customer reused prior work")
	}
	oldOutput, err := os.ReadFile(filepath.Join(first.work.OutputDir, first.artifact.Path))
	if err != nil || !bytes.Equal(oldOutput, first.data) || hash(oldOutput) != first.artifact.SHA256 {
		t.Fatal("later work overwrote the prior artifact")
	}
	oldInput, err := os.ReadFile(first.work.Inputs[0].Path)
	if err != nil || !bytes.Equal(oldInput, original) {
		t.Fatal("original spreadsheet was overwritten")
	}
	brief := documentParts("docx")
	reference := documentParts("docx")
	reference["word/document.xml"] = strings.Replace(reference["word/document.xml"], "Fictional reception brief", "Fictional approved venue: Cedar Hall", 1)
	document := run(makeMessage("document-one", "", "staff-b", core.FileAttachment{FileName: "brief.docx", Data: packageBytes(t, brief)}, core.FileAttachment{FileName: "selected-approved-reference.docx", Data: packageBytes(t, reference)}), fileJourneyStep{"brief.docx", "Fictional reception brief", "Fictional prepared reception plan", "docx", "selected-approved-reference.docx"})
	parts, err := fileJourneyParts(document.data)
	if err != nil || !strings.Contains(parts["word/document.xml"], "Fictional approved venue: Cedar Hall") {
		t.Fatal("generated DOCX did not use the selected fictional reference")
	}
	deliveries, _ := platform.snapshot()
	if len(deliveries) != 5 {
		t.Fatalf("recipient file count=%d, want 5", len(deliveries))
	}
	for i, delivery := range deliveries {
		want := fileJourneyRoute{"staff-a", "input-one"}
		if i == 3 {
			want.Thread = "customer-two"
		}
		if i == 4 {
			want = fileJourneyRoute{"staff-b", "document-one"}
		}
		if delivery.route != want || validateOOXML(delivery.file.FileName, delivery.file.Data) != nil {
			t.Fatal("delivery did not preserve original recipient and document")
		}
	}
	for _, msg := range []*core.Message{makeMessage("unknown-reply", "unknown-origin", "staff-a"), makeMessage("cross-person", first.artifact.MessageReceipt, "staff-b")} {
		if platform.authorize(*msg, msg.ParentMessageID) {
			t.Fatal("transport authorized an unbound reply")
		}
		before := shared.turns.Load()
		_, beforeReplies := platform.snapshot()
		engine.ReceiveMessage(platform, msg)
		after, afterReplies := platform.snapshot()
		if shared.turns.Load() != before || len(after) != 5 || len(afterReplies) != len(beforeReplies)+1 {
			t.Fatal("unbound reply did not stop before model and file delivery")
		}
	}
}
