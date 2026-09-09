package reminders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type delivery struct{ chat, text, id string }
type senderStub struct {
	calls []delivery
	err   error
	hook  func()
}

type revocableSender struct {
	senderStub
	allowed bool
}

func (s *revocableSender) ReminderAuthorized() bool { return s.allowed }

func TestLiveRevocationStopsToolsAndDelivery(t *testing.T) {
	h, _, now, _ := fixture(t)
	sender := &revocableSender{allowed: true}
	h.senders["a"] = sender
	create(t, h, "ag", "one", "test")
	sender.allowed = false
	if r := create(t, h, "ag", "two", "test"); r["code"] != "invalid_identity" {
		t.Fatal("revoked user created reminder")
	}
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(sender.calls) != 0 {
		t.Fatal("revoked identity received delivery")
	}
}

func TestUnwritableDatabaseDisablesReminderOperations(t *testing.T) {
	h, _, _, path := fixture(t)
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0600) }()
	if err := h.Tick(context.Background(), core.LangChinese); !errors.Is(err, ErrUnavailable) {
		t.Fatal("readonly state not rejected", err)
	}
}

func (s *senderStub) SendReminder(_ context.Context, c, text, id string) (string, error) {
	s.calls = append(s.calls, delivery{c, text, id})
	if s.hook != nil {
		s.hook()
	}
	if s.err != nil {
		return "", s.err
	}
	return "receipt", nil
}

func fixture(t *testing.T) (*Host, *senderStub, *time.Time, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX protected database boundary; exercised in Linux CI")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "reminders.sqlite")
	if err := Initialize(path); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := &senderStub{}
	h, err := New(store, []Route{{"a", "pa", "ap", "pa"}, {"a", "group", "ag", "pa"}, {"b", "pb", "bp", "pb"}, {"b", "group", "bg", "pb"}}, map[string]Sender{"a": s, "b": s})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, beijing)
	h.now = func() time.Time { return now }
	return h, s, &now, path
}

func principal(project, message string) core.ActionPrincipal {
	u, c := "a", "pa"
	if project == "ag" {
		c = "group"
	}
	if project == "bp" {
		u, c = "b", "pb"
	}
	if project == "bg" {
		u, c = "b", "group"
	}
	return core.ActionPrincipal{Platform: "feishu", UserID: u, ChatID: c, Project: project, SessionKey: "session", MessageID: message}
}

func call(t *testing.T, h *Host, cmd, project, message string, in map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(in)
	result, card, err := h.Tool(context.Background(), cmd, raw, principal(project, message), "", core.LangChinese)
	if err != nil {
		t.Fatal(err)
	}
	if card != nil {
		t.Fatal("reminder produced approval")
	}
	return result
}
func create(t *testing.T, h *Host, project, message, content string) map[string]any {
	return call(t, h, "reminder-create", project, message, map[string]any{"content": content, "at": h.now().Add(time.Hour).Format(time.RFC3339)})
}
func item(result map[string]any) map[string]any { return result["reminder"].(map[string]any) }

func TestCreateReplayExplicitRepeatAndScopedVisibility(t *testing.T) {
	h, _, _, _ := fixture(t)
	a := create(t, h, "ap", "one", "PRIVATE_CANARY")
	retry := create(t, h, "ap", "one", "PRIVATE_CANARY")
	if item(a)["id"] != item(retry)["id"] {
		t.Fatal("model retry duplicated reminder")
	}
	if item(create(t, h, "ap", "two", "PRIVATE_CANARY"))["id"] == item(a)["id"] {
		t.Fatal("new explicit message silently merged")
	}
	group := call(t, h, "reminder-list", "ag", "list", map[string]any{})
	data, _ := json.Marshal(group)
	if strings.Contains(string(data), "PRIVATE_CANARY") {
		t.Fatal("private body reached group model")
	}
	for _, project := range []string{"ag", "bp", "bg"} {
		r := call(t, h, "reminder-cancel", project, "cancel", map[string]any{"id": item(a)["id"], "version": 1})
		if r["code"] != "not_found" {
			t.Fatal("cross-scope cancel accepted")
		}
	}
	group = call(t, h, "reminder-list", "ag", "all", map[string]any{"all": true})
	data, _ = json.Marshal(group)
	if strings.Contains(string(data), "PRIVATE_CANARY") || group["status"] != "private_list_queued" {
		t.Fatal("all-list privacy failure")
	}
}

func TestVersionUpdateCancelAndPersistAcrossRestart(t *testing.T) {
	h, _, _, path := fixture(t)
	id := item(create(t, h, "ag", "one", "test"))["id"]
	r := call(t, h, "reminder-update", "ap", "edit", map[string]any{"id": id, "version": 1, "content": "changed"})
	if r["status"] != "updated" {
		t.Fatal(r)
	}
	if r = call(t, h, "reminder-cancel", "ap", "stale", map[string]any{"id": id, "version": 1}); r["code"] != "version_conflict" {
		t.Fatal(r)
	}
	_ = h.store.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h.store = store
	r = call(t, h, "reminder-cancel", "ap", "cancel", map[string]any{"id": id, "version": 2})
	if r["status"] != "cancelled" {
		t.Fatal(r)
	}
}

func TestOverduePrivateBatchRetryUUIDAndWindow(t *testing.T) {
	h, s, now, _ := fixture(t)
	create(t, h, "ag", "one", "first")
	create(t, h, "ap", "two", "second")
	*now = now.Add(2 * time.Hour)
	s.err = errors.New("timeout")
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 || s.calls[0].chat != "pa" || !strings.Contains(s.calls[0].text, "first") || !strings.Contains(s.calls[0].text, "second") || !strings.Contains(s.calls[0].text, "延迟") {
		t.Fatal("lost overdue aggregation")
	}
	*now = now.Add(time.Minute)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 2 || s.calls[0].id != s.calls[1].id || s.calls[0].text != s.calls[1].text {
		t.Fatal("retry payload changed")
	}
	*now = now.Add(time.Hour)
	s.err = nil
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 3 || s.calls[1].id == s.calls[2].id || !strings.Contains(s.calls[2].text, "可能重复") {
		t.Fatal("dedup window not handled")
	}
}

func TestCancelInFlightPreventsRetriesWithoutClaimingRecall(t *testing.T) {
	h, s, now, _ := fixture(t)
	id := item(create(t, h, "ap", "one", "test"))["id"]
	s.err = errors.New("timeout")
	s.hook = func() {
		r := call(t, h, "reminder-cancel", "ap", "cancel", map[string]any{"id": id, "version": 1})
		if r["may_be_in_flight"] != true {
			t.Fatal("false recall promise")
		}
		s.hook = nil
	}
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 {
		t.Fatal("cancelled reminder retried")
	}
}

func TestPermissionDeniedPausesWithoutAlternateDestination(t *testing.T) {
	h, s, now, _ := fixture(t)
	create(t, h, "ag", "one", "test")
	s.err = core.ErrReminderPermission
	*now = now.Add(time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(24 * time.Hour)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 || s.calls[0].chat != "pa" {
		t.Fatal("permission denial rerouted/retried")
	}
}

func TestDatabaseMissingCorruptAndDeletedFailClosed(t *testing.T) {
	h, _, _, path := fixture(t)
	if _, err := Open(path + ".missing"); err == nil {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(path + ".missing"); !os.IsNotExist(err) {
		t.Fatal("missing DB created")
	}
	_ = h.store.Close()
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt DB accepted")
	}
}

func TestReplayReceiptAfterDeadlineAndPrivatePagination(t *testing.T) {
	h, _, now, _ := fixture(t)
	body := map[string]any{"content": "fixed", "at": now.Add(time.Minute).Format(time.RFC3339)}
	first := call(t, h, "reminder-create", "ap", "fixed-message", body)
	*now = now.Add(time.Hour)
	retry := call(t, h, "reminder-create", "ap", "fixed-message", body)
	if retry["status"] != "created" || item(first)["id"] != item(retry)["id"] {
		t.Fatal("lost durable retry receipt")
	}
	for i := 0; i < 25; i++ {
		create(t, h, "ap", fmt.Sprintf("message-%d", i), strings.Repeat("<", 2000))
	}
	seen := map[string]bool{}
	request := map[string]any{}
	for {
		r := call(t, h, "reminder-list", "ap", "list", request)
		data, _ := json.Marshal(r)
		if len(data) > 1<<20 {
			t.Fatal("page exceeds client bound")
		}
		for _, row := range r["reminders"].([]map[string]any) {
			id := row["id"].(string)
			if seen[id] {
				t.Fatal("duplicate page item")
			}
			seen[id] = true
		}
		if r["next_id"] == nil {
			break
		}
		request = map[string]any{"id": r["next_id"]}
	}
	if len(seen) != 26 {
		t.Fatalf("lost rows: %d", len(seen))
	}
}

func TestCrossProcessCreateDeduplicatesAtomically(t *testing.T) {
	_, _, _, path := fixture(t)
	commands := []*exec.Cmd{}
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReminderProcessHelper$")
		cmd.Env = append(os.Environ(), "REMINDER_TEST_DB="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err = store.change(context.Background(), func(st *state) error {
		if len(st.Items) != 1 {
			t.Fatalf("duplicate across processes: %d", len(st.Items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReminderProcessHelper(t *testing.T) {
	path := os.Getenv("REMINDER_TEST_DB")
	if path == "" {
		return
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	h, err := New(store, []Route{{"a", "pa", "ap", "pa"}}, map[string]Sender{"a": &senderStub{}})
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return time.Date(2026, 9, 9, 0, 0, 0, 0, beijing) }
	create(t, h, "ap", "same-original-message", "same content")
}

func TestInputTimeAndForgedFields(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, beijing)
	for _, raw := range []string{`{"content":"x","at":"2026-09-14T09:00:00"}`, `{"content":"x","at":"2026-09-08T09:00:00+08:00"}`, `{"content":"x","content":"y","at":"2026-09-14T09:00:00+08:00"}`, `{"content":"x","recipient":"other","at":"2026-09-14T09:00:00+08:00"}`} {
		in, err := decode([]byte(raw))
		if err == nil {
			err = validateInput("reminder-create", in, now)
		}
		if err == nil {
			t.Fatal("unsafe input accepted", raw)
		}
	}
	in, err := decode([]byte(`{"content":"x","at":"2026-09-14T09:00:00+08:00"}`))
	if err != nil || validateInput("reminder-create", in, now) != nil {
		t.Fatal("valid input rejected")
	}
}
