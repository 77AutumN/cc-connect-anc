package reminders

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/actionhost/opsalerts"
	"github.com/chenhg5/cc-connect/core"
)

func TestHealthReadsOnlyVerifiedFailuresAndNeverPrivateContent(t *testing.T) {
	h, sender, now, path := fixture(t)
	create(t, h, "ag", "one", "DO-NOT-EXPOSE-PRIVATE-CONTENT")
	create(t, h, "ag", "two", "DO-NOT-EXPOSE-PRIVATE-CONTENT")
	if snap := h.HealthSnapshot(context.Background()); !snap.Complete || len(snap.Faults) != 0 {
		t.Fatal("healthy pending work flagged")
	}
	*now = now.Add(time.Hour)
	sender.err = errors.New("synthetic network failure")
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	snap := h.HealthSnapshot(context.Background())
	if !snap.Complete || len(snap.Faults) != 1 || snap.Faults[0].Category != opsalerts.Delivery || snap.Faults[0].Count != 2 || snap.Faults[0].Since != now.Unix() {
		t.Fatal("wrong aggregate", snap)
	}
	raw, _ := json.Marshal(snap)
	if strings.Contains(string(raw), "DO-NOT-EXPOSE") || strings.Contains(string(raw), `"pa"`) || snap.Faults[0].Scope != opsalerts.Scope("a") {
		t.Fatal("health leaks data")
	}
	sender.err = nil
	*now = now.Add(time.Minute)
	if err := h.Tick(context.Background(), core.LangChinese); err != nil {
		t.Fatal(err)
	}
	if snap := h.HealthSnapshot(context.Background()); !snap.Complete || len(snap.Faults) != 0 {
		t.Fatal("accepted receipt not reflected")
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0600) }()
	if snap := h.HealthSnapshot(context.Background()); snap.Complete || len(snap.Faults) != 1 || snap.Faults[0].Category != opsalerts.Storage {
		t.Fatal("store failure implied recovery")
	}
	var missing *Host
	if snap := missing.HealthSnapshot(context.Background()); snap.Complete || snap.Faults[0].Category != opsalerts.Storage {
		t.Fatal("startup failure unreported")
	}
}

func TestHealthDetectsFutureRouteRevocationWithoutWaitingForDueTime(t *testing.T) {
	h, _, _, _ := fixture(t)
	sender := &revocableSender{allowed: true}
	h.senders["a"] = sender
	create(t, h, "ap", "future", "private body")
	sender.allowed = false
	snap := h.HealthSnapshot(context.Background())
	if len(snap.Faults) != 1 || snap.Faults[0].Category != opsalerts.Route {
		t.Fatal("future revocation missed", snap)
	}
	if len(sender.calls) != 0 {
		t.Fatal("health sent reminder")
	}
}

func TestReminderPresentationContentFirstNumberLastAndNoVersion(t *testing.T) {
	h, _, now, _ := fixture(t)
	r := &Reminder{ID: "rem_fixture", Content: "准备虚构资料", At: now.Add(time.Hour).Unix(), Version: 12, Status: "paused"}
	line := h.reminderLine(r, core.LangChinese)
	if !strings.HasPrefix(line, r.Content+"\n") || !strings.HasSuffix(line, "编号：rem_fixture") {
		t.Fatal("unreadable reminder", line)
	}
	st := emptyState()
	h.queueList(&st, Route{User: "a", PrivateChat: "pa"}, []*Reminder{r}, core.LangChinese)
	for _, b := range st.Batches {
		if strings.Contains(b.Text, "v12") || !strings.Contains(b.Text, "暂停") || !strings.HasSuffix(b.Text, "编号：rem_fixture") {
			t.Fatal("internal status leaked", b.Text)
		}
	}
}
