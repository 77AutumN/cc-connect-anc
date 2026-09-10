package opsalerts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type sendCall struct{ chat, text, uuid string }
type senderStub struct {
	calls []sendCall
	err   error
	hook  func()
}

func (s *senderStub) SendReminder(_ context.Context, chat, text, id string) (string, error) {
	s.calls = append(s.calls, sendCall{chat, text, id})
	if s.hook != nil {
		s.hook()
	}
	if s.err != nil {
		return "", s.err
	}
	return "synthetic-receipt", nil
}

func logicFixture() (*Host, *time.Time, *senderStub, Recipient) {
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	sender := &senderStub{}
	r := Recipient{"fixed-binding", "private-fixture", sender}
	h := New(nil, func() Recipient { return r }, func(context.Context) Snapshot { return Snapshot{Complete: true} }, core.LangChinese)
	h.now = func() time.Time { return now }
	return h, &now, sender, r
}

func TestDeadlineHourlyDedupAndScopedRecovery(t *testing.T) {
	h, now, _, r := logicFixture()
	st := emptyState()
	snap := Snapshot{Faults: []Fault{{Delivery, "hashed-scope", 2, now.Unix()}}, Complete: true}
	h.reconcile(&st, snap, r)
	*now = now.Add(14*time.Minute + 59*time.Second)
	h.reconcile(&st, snap, r)
	if len(st.Messages) != 0 {
		t.Fatal("early alert")
	}
	*now = now.Add(time.Second)
	h.reconcile(&st, snap, r)
	if len(st.Messages) != 1 {
		t.Fatal("15 minute deadline depended on retry")
	}
	for i := 0; i < 1000; i++ {
		h.reconcile(&st, snap, r)
	}
	if len(st.Messages) != 1 {
		t.Fatal("duplicate observation generated messages")
	}
	*now = now.Add(time.Hour - time.Second)
	for _, m := range st.Messages {
		m.Receipt = "accepted"
	}
	h.reconcile(&st, snap, r)
	if len(st.Messages) != 1 {
		t.Fatal("early hourly summary")
	}
	*now = now.Add(time.Second)
	h.reconcile(&st, snap, r)
	if len(st.Messages) != 2 {
		t.Fatal("missing hourly summary")
	}
	h.reconcile(&st, Snapshot{Faults: []Fault{{Storage, "store", 1, 0}}}, r)
	if st.Events[string(Delivery)+":hashed-scope"].Closed {
		t.Fatal("DB loss cleared delivery failure")
	}
	h.reconcile(&st, Snapshot{Complete: true}, r)
	if !st.Events[string(Delivery)+":hashed-scope"].Closed {
		t.Fatal("verified recovery not recorded")
	}
}

func TestUnknownSendKeepsUUIDAndRecoveryOrdering(t *testing.T) {
	h, now, _, r := logicFixture()
	st := emptyState()
	h.reconcile(&st, Snapshot{Faults: []Fault{{Permission, "scope", 1, 0}}, Complete: true}, r)
	first := h.claim(&st, r)
	if first == nil {
		t.Fatal("immediate failure not queued")
	}
	h.reconcile(&st, Snapshot{Complete: true}, r)
	if h.claim(&st, r) != nil {
		t.Fatal("recovery overtook leased warning")
	}
	*now = now.Add(time.Minute)
	retry := h.claim(&st, r)
	if retry == nil || retry.UUID != first.UUID || retry.Text != first.Text {
		t.Fatal("unstable retry")
	}
	*now = now.Add(time.Hour)
	later := h.claim(&st, r)
	if later.UUID == first.UUID || !strings.Contains(later.Text, "可能重复") {
		t.Fatal("dedup window uncertainty hidden")
	}
	changed := r
	changed.Binding = "new-owner-binding"
	if h.claim(&st, changed) != nil {
		t.Fatal("old message rerouted")
	}
	st.Messages[first.ID].Receipt = "receipt"
	if m := h.claim(&st, r); m == nil || m.ID == first.ID {
		t.Fatal("recovery not released after receipt")
	}
}

func TestIncidentBindingIsFrozenBeforeThresholdAndDegradedSourceCannotClearIt(t *testing.T) {
	h, now, _, r := logicFixture()
	st := emptyState()
	snap := Snapshot{Faults: []Fault{{Delivery, "scope", 1, now.Unix()}}, Complete: true}
	h.reconcile(&st, snap, r)
	changed := r
	changed.Binding = "different-owner"
	changed.Chat = "other-private"
	*now = now.Add(20 * time.Minute)
	h.reconcile(&st, snap, changed)
	if len(st.Messages) != 0 {
		t.Fatal("threshold rerouted earlier incident")
	}
	h.reconcile(&st, snap, r)
	if len(st.Messages) != 1 {
		t.Fatal("original binding lost")
	}
	*now = now.Add(4 * time.Hour)
	h.reconcile(&st, snap, r)
	if len(st.Messages) != 1 {
		t.Fatal("offline hourly backlog not coalesced")
	}
	h.reconcile(&st, Snapshot{Complete: false}, r)
	if st.Events[string(Delivery)+":scope"].Closed {
		t.Fatal("unknown source reported recovered")
	}
}

func durableFixture(t *testing.T) (*Host, *time.Time, *senderStub, *Snapshot, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes: exercised by Linux test binary/CI")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alerts.sqlite")
	if err := Initialize(path); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h, now, sender, _ := logicFixture()
	h.store = s
	snap := &Snapshot{Faults: []Fault{{Permission, "scope", 1, 0}}, Complete: true}
	h.snapshot = func(context.Context) Snapshot { return *snap }
	return h, now, sender, snap, path
}

func TestDurableRetryRestartAndPermissionPause(t *testing.T) {
	h, now, sender, snap, path := durableFixture(t)
	sender.err = errors.New("synthetic timeout")
	h.Tick(context.Background())
	if len(sender.calls) != 1 {
		t.Fatal("not sent")
	}
	first := sender.calls[0]
	_ = h.store.Close()
	var err error
	h.store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.store.Close() }()
	*now = now.Add(time.Minute)
	h.Tick(context.Background())
	if len(sender.calls) != 2 || sender.calls[1] != first {
		t.Fatal("restart changed frozen send")
	}
	*now = now.Add(5 * time.Minute)
	sender.err = core.ErrReminderPermission
	h.Tick(context.Background())
	*now = now.Add(time.Hour)
	h.Tick(context.Background())
	if len(sender.calls) != 3 {
		t.Fatal("explicit permission rejection retried")
	}
	*snap = Snapshot{Complete: true}
	h.Tick(context.Background())
	if len(sender.calls) != 3 {
		t.Fatal("recovery bypassed paused send")
	}
}

func TestRestartPreservesFirstFailureDeadline(t *testing.T) {
	h, now, sender, snap, path := durableFixture(t)
	*snap = Snapshot{Faults: []Fault{{Delivery, "scope", 1, now.Unix()}}, Complete: true}
	h.Tick(context.Background())
	*now = now.Add(14 * time.Minute)
	_ = h.store.Close()
	var err error
	h.store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.store.Close() }()
	h.Tick(context.Background())
	if len(sender.calls) != 0 {
		t.Fatal("early alert")
	}
	*now = now.Add(time.Minute)
	h.Tick(context.Background())
	if len(sender.calls) != 1 {
		t.Fatal("restart reset deadline")
	}
}

func TestAlertStoreFailureIsBoundedAndNeverReinitializes(t *testing.T) {
	h, now, sender, _, path := durableFixture(t)
	if Initialize(path) == nil {
		t.Fatal("existing DB reinitialized")
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0600) }()
	for i := 0; i < 20; i++ {
		h.Tick(context.Background())
		*now = now.Add(time.Hour)
	}
	if len(sender.calls) != 3 || !strings.Contains(sender.calls[0].text, "尽力通知") {
		t.Fatal("unbounded or silent degradation", len(sender.calls))
	}
	if _, err := Open(filepath.Join(filepath.Dir(path), "missing.sqlite")); err == nil {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "missing.sqlite")); !os.IsNotExist(err) {
		t.Fatal("missing DB recreated")
	}
}

func TestReceiptWriteFailurePreservesClaimAndReportsDegraded(t *testing.T) {
	h, _, sender, _, path := durableFixture(t)
	sender.hook = func() { sender.hook = nil; _ = os.Chmod(path, 0400) }
	h.Tick(context.Background())
	_ = os.Chmod(path, 0600)
	if len(sender.calls) != 2 {
		t.Fatal("missing bounded fallback notice")
	}
	_ = h.store.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err = s.change(context.Background(), func(st *state) error {
		for _, m := range st.Messages {
			if m.Attempts != 1 || m.Receipt != "" || m.Lease == 0 {
				t.Fatal("uncertain send claim erased")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptOutboxFailsClosed(t *testing.T) {
	_, _, _, _, path := durableFixture(t)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corruption reset")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "corrupt" {
		t.Fatal("corruption overwritten")
	}
}
