package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestBypassPreservesBusinessQuestionAndActualAnswer(t *testing.T) {
	for _, mode := range []string{"bypassPermissions", "dontAsk"} {
		t.Run(mode, func(t *testing.T) {
			stdin := &imageTestStdin{}
			cs := &claudeSession{stdin: stdin, events: make(chan core.Event, 2), ctx: context.Background()}
			cs.alive.Store(true)
			cs.setPermissionMode(mode)
			input := map[string]any{"questions": []any{map[string]any{
				"question": "Which fictional package should the draft use?",
				"header":   "Package", "options": []any{map[string]any{"label": "A", "description": "Base"}, map[string]any{"label": "B", "description": "Extended"}},
			}}}
			cs.handleControlRequest(map[string]any{"request_id": "question-fixture", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "input": input}})
			if stdin.Len() != 0 {
				t.Fatal("business question was automatically answered before the employee replied")
			}
			select {
			case event := <-cs.events:
				if event.Type != core.EventPermissionRequest || len(event.Questions) != 1 || event.RequestID != "question-fixture" {
					t.Fatalf("question event = %#v", event)
				}
			default:
				t.Fatal("employee never received the business question")
			}
			input["answers"] = map[string]string{"Which fictional package should the draft use?": "B"}
			if err := cs.RespondPermission("question-fixture", core.PermissionResult{Behavior: "allow", UpdatedInput: input}); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Response struct {
					RequestID string `json:"request_id"`
					Response  struct {
						Behavior string         `json:"behavior"`
						Input    map[string]any `json:"updatedInput"`
					} `json:"response"`
				} `json:"response"`
			}
			if err := json.Unmarshal(stdin.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Response.RequestID != "question-fixture" || result.Response.Response.Behavior != "allow" || result.Response.Response.Input["answers"].(map[string]any)["Which fictional package should the draft use?"] != "B" {
				t.Fatal("actual employee answer was not returned to Claude")
			}
		})
	}
}

func TestBypassResidualTechnicalRequestIsDeniedWithoutEmployeeCard(t *testing.T) {
	stdin := &imageTestStdin{}
	cs := &claudeSession{stdin: stdin, events: make(chan core.Event, 2), ctx: context.Background()}
	cs.alive.Store(true)
	cs.setPermissionMode("bypassPermissions")
	cs.handleControlRequest(map[string]any{"request_id": "restricted-fixture", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "fixture-needs-explicit-permission"}}})
	var result struct {
		Response struct {
			Response struct {
				Behavior string `json:"behavior"`
				Message  string `json:"message"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(stdin.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Response.Response.Behavior != "deny" || result.Response.Response.Message == "" {
		t.Fatal("native bypass exception was silently approved")
	}
	if len(cs.events) != 0 {
		t.Fatal("technical permission escaped to the employee UI")
	}
	if err := cs.ctx.Err(); err != nil {
		t.Fatalf("a denied tool must allow Claude to continue: %v", err)
	}
}

type permissionBrokenStdin struct{}

func (permissionBrokenStdin) Write([]byte) (int, error) { return 0, errors.New("fixture broken pipe") }
func (permissionBrokenStdin) Close() error              { return nil }

func TestBypassDenialWriteFailureEndsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := &claudeSession{stdin: permissionBrokenStdin{}, events: make(chan core.Event, 2), ctx: ctx, cancel: cancel}
	cs.alive.Store(true)
	cs.setPermissionMode("bypassPermissions")
	cs.handleControlRequest(map[string]any{"request_id": "broken-fixture", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "fixture-needs-explicit-permission"}}})
	if ctx.Err() == nil {
		t.Fatal("failed denial left Claude waiting indefinitely")
	}
	select {
	case event := <-cs.events:
		if event.Type != core.EventError || event.Error == nil {
			t.Fatalf("failure event = %#v", event)
		}
	default:
		t.Fatal("denial write failure was not reported")
	}
}

func TestBypassModeChangesRequireNativeCLIRestart(t *testing.T) {
	for _, pair := range [][2]string{{"default", "bypassPermissions"}, {"bypassPermissions", "default"}, {"bypassPermissions", "acceptEdits"}, {"bypassPermissions", "dontAsk"}} {
		cs := &claudeSession{}
		cs.setPermissionMode(pair[0])
		if cs.SetLiveMode(pair[1]) || cs.permissionModeValue() != pair[0] {
			t.Fatalf("%s -> %s must restart native CLI", pair[0], pair[1])
		}
	}
}

func TestBypassDenialWriteFailureFullQueueStillCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := &claudeSession{stdin: permissionBrokenStdin{}, events: make(chan core.Event), ctx: ctx, cancel: cancel}
	cs.alive.Store(true)
	cs.setPermissionMode("bypassPermissions")
	done := make(chan struct{})
	go func() {
		cs.handleControlRequest(map[string]any{"request_id": "blocked-fixture", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "fixture"}}})
		close(done)
	}()
	select {
	case <-done:
		if ctx.Err() == nil {
			t.Fatal("failed denial did not cancel the process")
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("permission failure waits for an event consumer before cancelling")
	}
}
