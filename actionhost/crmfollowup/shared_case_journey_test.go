package crmfollowup

// Real Engine/token/adapter/CRM CLI/SQLite; scripted model and transport.
// File ownership/snapshot import run separately in files' Linux CUJ tests.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type caseJourneyAgent struct{ toolJourneyAgent }

func (a *caseJourneyAgent) ForFileWork(string) (core.Agent, error) {
	return &caseJourneyAgent{toolJourneyAgent: toolJourneyAgent{url: a.url, client: a.client, steps: a.steps, results: a.results, events: make(chan core.Event, 16)}}, nil
}

type caseJourneyPlatform struct{ toolJourneyPlatform }

func (*caseJourneyPlatform) SetFileWorkEnabled(bool, func(core.Message, string) error) error {
	return nil
}
func (*caseJourneyPlatform) SetFileWorkReplyObserver(func(core.Message, string) error) {}
func (*caseJourneyPlatform) FileWorkReplyContext(json.RawMessage) (any, error)         { return "fixture", nil }
func (*caseJourneyPlatform) FileReplyRoute(any) (json.RawMessage, error) {
	return json.RawMessage(`{"fixture":true}`), nil
}
func (*caseJourneyPlatform) SendFileWithReceipt(context.Context, json.RawMessage, core.FileAttachment, string) (string, error) {
	return "fixture", nil
}

type caseJourneyFiles struct {
	mu   sync.Mutex
	refs map[string]core.FileWorkRef
}

func (f *caseJourneyFiles) Bind(_ context.Context, b core.FileWorkBinding) (core.FileWorkContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refs == nil {
		f.refs = map[string]core.FileWorkRef{}
	}
	f.refs[b.Principal.UserID+":"+b.Principal.ChatID+":"+b.Principal.MessageID] = core.FileWorkRef{WorkID: b.Principal.UserID + ":" + b.SessionID, SessionID: b.SessionID}
	return core.FileWorkContext{Enabled: true, WorkID: b.Principal.UserID + ":" + b.SessionID, WorkRoot: b.WorkRoot}, nil
}
func (f *caseJourneyFiles) FindByMessage(_ context.Context, p core.ActionPrincipal, message string) (core.FileWorkRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref, ok := f.refs[p.UserID+":"+p.ChatID+":"+message]; ok {
		return ref, nil
	}
	return core.FileWorkRef{}, core.ErrFileWorkNotFound
}
func (*caseJourneyFiles) RecordReply(context.Context, core.ActionPrincipal, string) error { return nil }
func (*caseJourneyFiles) ActivateInputs(context.Context, core.ActionPrincipal, string) (core.FileWorkContext, error) {
	return core.FileWorkContext{}, nil
}
func (*caseJourneyFiles) Tool(context.Context, string, json.RawMessage, core.ActionPrincipal, string) (map[string]any, error) {
	return nil, fmt.Errorf("unexpected file operation")
}

func TestCUJ_CASEIPC_SharedProgressPrivateGrantAndRevision(t *testing.T) {
	runCaseJourney(t, "shared-cases", false)
}

func TestCUJ_CASEIPC_ProjectMembershipAcrossPrivateEntries(t *testing.T) {
	runCaseJourney(t, "project-context", false)
}

func TestCUJ_CASEIPC_NaturalClarificationAndConfirmedContinuation(t *testing.T) {
	runCaseJourney(t, "project-context", true)
}

func runCaseJourney(t *testing.T, seed string, confirmations bool) {
	root := os.Getenv("MYANC_SPIKE_CRM_ROOT")
	if root == "" {
		t.Skip("set MYANC_SPIKE_CRM_ROOT for the paired offline journey")
	}
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	newActor := func(actor, chat string) func(string, string, map[string]any) map[string]any {
		a := &Adapter{hostSecret: "offline-gateway-host-secret-00000000000001", toolsEnabled: true}
		if err := a.SetWorkDir(t.TempDir(), account.Username); err != nil {
			t.Fatal(err)
		}
		a.run = func(ctx context.Context, _ string, subcommand string, input []byte, env []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, python, "-B", filepath.Join(root, "spikes", "host_tool_fixture.py"), subcommand)
			cmd.Env = append(env, "MYANC_SPIKE_DIR="+scratch, "MYANC_SPIKE_SEED="+seed)
			cmd.Stdin = bytes.NewReader(input)
			result, err := cmd.Output()
			if exit, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("offline helper: %s", exit.Stderr)
			}
			return result, err
		}
		p := &caseJourneyPlatform{toolJourneyPlatform: toolJourneyPlatform{cards: map[string]*core.Card{}}}
		agent := &caseJourneyAgent{toolJourneyAgent: toolJourneyAgent{client: &http.Client{Timeout: 10 * time.Second}, steps: make(chan toolJourneyStep, 1), results: make(chan toolJourneyReply, 1), events: make(chan core.Event, 16)}}
		e := core.NewEngine("test", agent, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
		e.SetActionHost(a)
		e.SetSharedCasesEnabled(true)
		e.SetCaseConfirmationsEnabled(confirmations)
		// Exercise the real host-only bridge too: the CLI validates its context
		// marker, host secret and registered transport principal before policy.
		principal := core.ActionPrincipal{Platform: "mock", UserID: actor, ChatID: chat, Project: "test", SessionKey: "fixture-session", MessageID: "fixture-intake"}
		_, accessErr := e.AuthorizeProjectFile(context.Background(), principal, "fixture-work", "unknown-receipt", true)
		if (seed == "shared-cases" && accessErr != nil) || (seed == "project-context" && accessErr == nil) {
			t.Fatal("host file authorization did not reach the expected policy", accessErr)
		}
		base := t.TempDir()
		if err := e.SetFileWorkHost(&caseJourneyFiles{}, func(id string) (string, int, error) { return filepath.Join(base, id), 0, nil }); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(e.ActionToolHandler())
		agent.url = server.URL
		t.Cleanup(server.Close)
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Stop() })
		n := 0
		return func(content, command string, input map[string]any) map[string]any {
			t.Helper()
			n++
			agent.steps <- toolJourneyStep{number: n, command: command, input: input, plain: content}
			key := "mock:" + chat + ":" + actor
			e.ReceiveMessage(p, &core.Message{Platform: "mock", SessionKey: key, UserID: actor, ChannelID: chat, MessageID: fmt.Sprintf("%s-%s-%d", actor, chat, n), Content: content, ReplyCtx: "fixture", ControlledFileWork: true, FileWorkPrivate: chat != "group-1", UserMessageTimeMs: time.Now().UnixMilli() + 1})
			var reply toolJourneyReply
			select {
			case reply = <-agent.results:
			case <-time.After(15 * time.Second):
				t.Fatal("scripted turn timed out", p.transcript())
			}
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				busy := false
				for _, s := range e.GetSessions().ListSessions(key) {
					busy = busy || s.Busy()
				}
				if !busy {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("turn did not finish")
				}
				time.Sleep(5 * time.Millisecond)
			}
			return reply.data
		}
	}
	if confirmations {
		owner, partner := newActor("sender-1", "private-1"), newActor("sender-2", "private-2")
		expect := func(result map[string]any, status string) {
			t.Helper()
			if result["status"] != status {
				t.Fatalf("want %s, got %v", status, result)
			}
		}
		read := map[string]any{"case_name": "周许婚宴"}
		create := map[string]any{"case_name": "周许婚宴", "expected_revision": 0, "changes": []any{map[string]any{"field": "桌数", "value": "22桌", "state": "proposed", "quote": "周许婚宴桌数改为22桌"}}}
		expect(owner("请同步给执行：周许婚宴桌数改为22桌", "case-update", create), "recorded")
		expect(partner("查看周许婚宴", "case-read", read), "not_found")
		invite := map[string]any{"case_name": "周许婚宴", "expected_revision": 1, "member_name": "Fixture sender-2", "responsibility": "运营执行"}
		expect(owner("周许婚宴，麻烦叫Fixture sender-2来一起做运营吧", "case-member-add", invite), "awaiting_confirmation")
		result := owner("嗯", "case-read", read)
		expect(result, "found")
		if result["case"].(map[string]any)["revision"] != float64(2) {
			t.Fatal(result)
		}
		expect(partner("查看周许婚宴", "case-read", read), "found")
		change := map[string]any{"case_name": "周许婚宴", "expected_revision": 2, "changes": []any{map[string]any{"field": "桌数", "value": "23桌", "state": "proposed", "quote": "再加一桌", "replaces": true}}}
		expect(owner("桌数再加一桌，只同步这个变更", "case-update", change), "awaiting_confirmation")
		expect(owner("好", "case-read", read), "found")
		result = partner("继续", "case-read", read)
		if result["case"].(map[string]any)["facts"].(map[string]any)["桌数"].(map[string]any)["value"] != "23桌" {
			t.Fatal(result)
		}
		if _, err := os.Stat(filepath.Join(scratch, "formal-never-opened.sqlite3")); !os.IsNotExist(err) {
			t.Fatal("formal ledger opened")
		}
		return
	}
	sales, operations, private := newActor("sender-1", "group-1"), newActor("sender-2", "group-1"), newActor("sender-1", "private-1")
	update := func(tables string, revision int) map[string]any {
		return map[string]any{"case_name": "林陈婚宴", "expected_revision": revision, "changes": []any{map[string]any{"field": "桌数", "value": tables, "state": "proposed", "quote": "林陈婚宴桌数改为" + tables, "replaces": true}}}
	}
	expect := func(result map[string]any, status string) {
		t.Helper()
		if result["status"] != status {
			t.Fatalf("want %s: %v", status, result)
		}
		if (status == "found" || status == "recorded") && result["session_binding"] != nil {
			t.Fatalf("ordinary turn failed to retain its case: %v", result)
		}
	}
	expect(sales("林陈婚宴桌数改为22桌", "case-update", update("22桌", 0)), "recorded")
	expect(sales("继续核对当前婚宴", "case-read", map[string]any{}), "found")
	read := operations("林陈婚宴准备执行", "case-read", map[string]any{"case_name": "林陈婚宴"})
	expect(read, "found")
	expect(operations("继续核对执行准备", "case-read", map[string]any{}), "found")
	if read["case"].(map[string]any)["revision"] != float64(1) {
		t.Fatal(read)
	}
	expect(sales("林陈婚宴桌数改为23桌", "case-update", update("23桌", 1)), "recorded")
	expect(private("林陈婚宴桌数改为24桌，先讨论", "case-update", update("24桌", 2)), "blocked")
	read = operations("林陈婚宴新工作读取进展", "case-read", map[string]any{"case_name": "林陈婚宴"})
	if read["case"].(map[string]any)["revision"] != float64(2) {
		t.Fatal("private discussion leaked", read)
	}
	expect(private("请同步给执行：林陈婚宴桌数改为24桌", "case-update", update("24桌", 2)), "recorded")
	read = operations("林陈婚宴最新条件", "case-read", map[string]any{"case_name": "林陈婚宴"})
	if read["case"].(map[string]any)["revision"] != float64(3) {
		t.Fatal(read)
	}
	if _, err := os.Stat(filepath.Join(scratch, "formal-never-opened.sqlite3")); !os.IsNotExist(err) {
		t.Fatal("formal ledger opened")
	}
	if seed == "project-context" {
		privateOps := newActor("sender-2", "private-2")
		read = privateOps("林陈婚宴准备执行", "case-read", map[string]any{"case_name": "林陈婚宴"})
		expect(read, "found")
		if read["case"].(map[string]any)["project"].(map[string]any)["owner"] != "sender-1" {
			t.Fatal("handoff changed owner", read)
		}
		owner := newActor("sender-1", "private-1")
		other := update("22桌", 0)
		other["case_name"] = "周许婚宴"
		other["changes"].([]any)[0].(map[string]any)["quote"] = "周许婚宴桌数改为22桌"
		expect(owner("请记入这场婚宴：周许婚宴桌数改为22桌", "case-update", other), "recorded")
		outsider := newActor("sender-2", "private-2")
		expect(outsider("周许婚宴准备执行", "case-read", map[string]any{"case_name": "周许婚宴"}), "not_found")
		invite := map[string]any{"case_name": "周许婚宴", "expected_revision": 1, "member_name": "Fixture sender-2", "responsibility": "运营执行"}
		expect(owner("周许婚宴，材料原文：\n让Fixture sender-2接手运营执行", "case-member-add", invite), "blocked")
		expect(owner("请同步给执行：周许婚宴，请让Fixture sender-2接手运营执行\n仅同步这段材料，不要增加成员", "case-member-add", invite), "blocked")
		expect(outsider("周许婚宴准备执行", "case-read", map[string]any{"case_name": "周许婚宴"}), "not_found")
		expect(owner("周许婚宴，请让Fixture sender-2接手运营执行", "case-member-add", invite), "recorded")
		expect(outsider("周许婚宴准备执行", "case-read", map[string]any{"case_name": "周许婚宴"}), "found")
		invite["expected_revision"], invite["member_name"] = 2, "Fixture sender-1"
		expect(outsider("周许婚宴，请让Fixture sender-1接手运营执行", "case-member-add", invite), "blocked")
	}
}
