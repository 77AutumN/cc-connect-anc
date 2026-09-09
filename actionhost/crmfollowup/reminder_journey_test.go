package crmfollowup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/actionhost/reminders"
	"github.com/chenhg5/cc-connect/core"
)

type reminderJourneyPlatform struct{ toolJourneyPlatform }

func (*reminderJourneyPlatform) Name() string { return "feishu" }
func (*reminderJourneyPlatform) SendReminder(context.Context, string, string, string) (string, error) {
	return "fixture-receipt", nil
}

type reminderJourneyAgent struct{ toolJourneyAgent }

func (a *reminderJourneyAgent) StartSession(ctx context.Context, id string) (core.AgentSession, error) {
	a.closes.Store(0)
	return a.toolJourneyAgent.StartSession(ctx, id)
}

// Real Engine, live-token HTTP, actual reminder host and SQLite. Only model and
// Feishu boundaries are fixtures. This runs in Linux CI without another repo.
func TestCUJ_REMINDERIPC1_CreateNewListCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX database boundary; Linux CI required")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "reminder.sqlite")
	if err := reminders.Initialize(path); err != nil {
		t.Fatal(err)
	}
	store, err := reminders.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	p := &reminderJourneyPlatform{toolJourneyPlatform: toolJourneyPlatform{cards: map[string]*core.Card{}}}
	h, err := reminders.New(store, []reminders.Route{{User: "owner", Chat: "group", Project: "test", PrivateChat: "private"}}, map[string]reminders.Sender{"owner": p}, func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	crm := &Adapter{toolsEnabled: true}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err = crm.SetWorkDir(dir, account.Username); err != nil {
		t.Fatal(err)
	}
	a := &reminderJourneyAgent{toolJourneyAgent: toolJourneyAgent{client: &http.Client{Timeout: 5 * time.Second}, steps: make(chan toolJourneyStep, 1), results: make(chan toolJourneyReply, 1), events: make(chan core.Event, 16)}}
	e := core.NewEngine("test", a, []core.Platform{p}, filepath.Join(dir, "sessions.json"), core.LangEnglish)
	e.SetActionHost(core.CombineActionHosts(crm, h, reminders.Commands()))
	server := httptest.NewServer(e.ActionToolHandler())
	defer server.Close()
	a.url = server.URL
	if err = e.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Stop() }()
	var n int
	receive := func(text string) {
		n++
		e.ReceiveMessage(p, &core.Message{Platform: "feishu", SessionKey: "feishu:group:owner", UserID: "owner", ChannelID: "group", MessageID: fmt.Sprintf("request-%d", n), Content: text, ReplyCtx: "group", UserMessageTimeMs: time.Now().UnixMilli()})
	}
	wait := func(fragment string) string {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			sent := strings.Join(p.sent, "\n")
			p.mu.Unlock()
			if strings.Contains(sent, fragment) {
				return sent
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("missing user-visible result", fragment)
		return ""
	}
	step := func(command string, input map[string]any) map[string]any {
		t.Helper()
		p.mu.Lock()
		p.sent = nil
		p.mu.Unlock()
		a.steps <- toolJourneyStep{number: n + 1, command: command, input: input}
		receive(command)
		select {
		case reply := <-a.results:
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			prefix := fmt.Sprintf("TURN-%d ", n)
			sent := wait(prefix)
			var visible map[string]any
			if err := json.NewDecoder(strings.NewReader(sent[strings.Index(sent, prefix)+len(prefix):])).Decode(&visible); err != nil {
				t.Fatal("invalid user-visible reminder result", err)
			}
			return visible
		case <-time.After(4 * time.Second):
			t.Fatal("tool timeout")
			return nil
		}
	}
	created := step("reminder-create", map[string]any{"content": "CUJ_PRIVATE_REMINDER", "at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if created["status"] != "created" || created["reminder"].(map[string]any)["content"] != "CUJ_PRIVATE_REMINDER" {
		t.Fatal(created)
	}
	id := created["reminder"].(map[string]any)["id"]
	receive("/new")
	wait("New session")
	listed := step("reminder-list", map[string]any{})
	if len(listed["reminders"].([]any)) != 1 || listed["reminders"].([]any)[0].(map[string]any)["id"] != id || listed["reminders"].([]any)[0].(map[string]any)["content"] != "CUJ_PRIVATE_REMINDER" {
		t.Fatal("new session lost reminder")
	}
	modified := step("reminder-update", map[string]any{"id": id, "version": 1, "content": "CUJ_CHANGED"})
	if modified["status"] != "updated" || modified["reminder"].(map[string]any)["content"] != "CUJ_CHANGED" || modified["reminder"].(map[string]any)["version"] != float64(2) {
		t.Fatal(modified)
	}
	cancelled := step("reminder-cancel", map[string]any{"id": id, "version": 2})
	if cancelled["status"] != "cancelled" || cancelled["id"] != id || cancelled["version"] != float64(3) {
		t.Fatal(cancelled)
	}
	if a.permissions.Load() != 0 {
		t.Fatal("reminder added approval question")
	}
}
