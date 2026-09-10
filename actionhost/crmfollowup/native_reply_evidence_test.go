package crmfollowup

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	mdtext "github.com/yuin/goldmark/text"
)

// Native evidence is measured at the platform boundary, including clarification
// cards that are not an assistant message in Session.GetHistory.
func captureNativeTurn(p *toolJourneyPlatform, message string, turn func(string) []realCanaryObservation, evidence *[]map[string]any) (outcomes []realCanaryObservation, shown map[string]any) {
	p.mu.Lock()
	visibleBefore, questionsBefore := len(p.sent), len(p.questionUI)
	p.mu.Unlock()
	returned := false
	defer func() {
		p.mu.Lock()
		visible := append([]string(nil), p.sent[visibleBefore:]...)
		questions := append([]string(nil), p.questionUI[questionsBefore:]...)
		p.mu.Unlock()
		shown = map[string]any{"user": message, "calls": nativeToolCalls(outcomes),
			"reply": strings.Join(visible, "\n"), "visible_messages": visible,
			"questions": questions, "awaiting_user": len(questions) > 0,
			"tool_results_returned": returned}
		// Record style separately. Acceptance still checks actual task effects,
		// authorization and truthful results; wording alone does not fail a turn.
		shown["wording_warning"] = nativeReplySmokeIssue(text(shown["reply"]))
		if !returned {
			// Goexit prevents returning outcomes. The turn's own deferred journal
			// retains actual host calls; an empty returned slice is not zero calls.
			shown["calls"] = nil
			shown["host_calls_evidence"] = "native-tool-turns.json"
		}
		*evidence = append(*evidence, shown)
	}()
	outcomes = turn(message)
	returned = true
	return outcomes, nil
}

func nativeToolCalls(outcomes []realCanaryObservation) []map[string]any {
	calls := make([]map[string]any, 0, len(outcomes))
	for _, outcome := range outcomes {
		calls = append(calls, map[string]any{"command": outcome.command, "input": outcome.input, "result": outcome.data})
	}
	return calls
}

func nativeReplyHasLink(reply, want string) bool {
	found := false
	doc := goldmark.DefaultParser().Parse(mdtext.NewReader([]byte(reply)))
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := node.(*ast.Link); entering && ok && string(link.Destination) == want {
			found = true
		}
		return ast.WalkContinue, nil
	})
	return found
}

func TestNativeTurnEvidenceSurvivesUnexpectedQuestionAbort(t *testing.T) {
	p := &toolJourneyPlatform{}
	var evidence []map[string]any
	done := make(chan struct{})
	go func() {
		defer close(done)
		captureNativeTurn(p, "只给我表格入口", func(string) []realCanaryObservation {
			card := core.NewCard().Title(core.NewI18n(core.LangChinese).T(core.MsgAskQuestionTitle), "blue").Markdown("要查看哪个客户？").Build()
			_ = p.ReplyCard(context.Background(), nil, card)
			runtime.Goexit() // testing.Fatal uses Goexit; the failed round must survive.
			return nil
		}, &evidence)
	}()
	<-done
	if len(evidence) != 1 || evidence[0]["awaiting_user"] != true || !strings.Contains(text(evidence[0]["reply"]), "要查看哪个客户") {
		t.Fatalf("aborted native round lost its actual question: %v", evidence)
	}
	if evidence[0]["tool_results_returned"] != false || evidence[0]["calls"] != nil || evidence[0]["host_calls_evidence"] != "native-tool-turns.json" {
		t.Fatal("aborted turn must identify the host journal rather than claim zero tool calls")
	}
}

func TestNativeNavigationLinkUsesActualDestination(t *testing.T) {
	want := "https://example.invalid/base/fixture"
	for _, tc := range []struct {
		reply    string
		accepted bool
	}{
		{"[打开 CRM](https://example.invalid/base/fixture)", true},
		{"[打开 CRM](<https://example.invalid/base/fixture>)", true},
		{"`[打开 CRM](https://example.invalid/base/fixture)`", false},
		{"[https://example.invalid/base/fixture](https://wrong.invalid/base/other)", false},
		{"[打开 CRM](https://example.invalid/base/fixture-other)", false},
	} {
		if nativeReplyHasLink(tc.reply, want) != tc.accepted {
			t.Errorf("incorrect link acceptance: %q", tc.reply)
		}
	}
}

// Only a smoke check for the synthetic Chinese cases, not a semantic grader or
// a runtime reply rewriter. Legitimate business names/IDs remain untouched.
func nativeReplySmokeIssue(reply string) bool {
	for _, token := range []string{"next_id", "private_list_queued", "owner_ref", "No更多分页"} {
		if strings.Contains(reply, token) {
			return true
		}
	}
	return false
}

func TestNativeTurnEvidenceIncludesVisibleQuestions(t *testing.T) {
	p := &toolJourneyPlatform{}
	var evidence []map[string]any
	_ = p.Reply(context.Background(), nil, "上一次回复")
	_, got := captureNativeTurn(p, "晚点提醒我", func(message string) []realCanaryObservation {
		_ = p.Reply(context.Background(), nil, "请补充具体时间。")
		question := core.NewCard().Title(core.NewI18n(core.LangChinese).T(core.MsgAskQuestionTitle), "blue").Markdown("你希望几点提醒？").Build()
		_ = p.ReplyCard(context.Background(), nil, question)
		return []realCanaryObservation{{command: "reminder-list", data: map[string]any{"status": "ok"}}}
	}, &evidence)
	reply, _ := got["reply"].(string)
	if got["user"] != "晚点提醒我" || !strings.Contains(reply, "请补充具体时间。") || !strings.Contains(reply, "你希望几点提醒？") || strings.Contains(reply, "上一次回复") || got["awaiting_user"] != true {
		t.Fatalf("actual visible reply/question missing or old turn included: %v", got)
	}
	questions, _ := got["questions"].([]string)
	if len(questions) != 1 || questions[0] == "" {
		t.Fatal("question card not captured")
	}
}

func TestNativeReplySmokeFlagsObservedPaginationNarration(t *testing.T) {
	for _, tc := range []struct {
		reply string
		issue bool
	}{
		{"No更多分页，两条都是待处理状态。", true},
		{"No more `next_id`, so this is the full result — all pages read.", true},
		{"状态为 private_list_queued，已排队。", true},
		{"请提供 owner_ref。", true},
		{"你还有 2 条未结束提醒：\n提醒编号：rem_example", false},
		{"已安排将完整清单发到你与我的私聊，请在那里查看。", false},
		{"CRM 当前记录显示客户名称为 Notion，识别码 TEST-0910。", false},
	} {
		if nativeReplySmokeIssue(tc.reply) != tc.issue {
			t.Errorf("smoke check disagrees with observed example: %q", tc.reply)
		}
	}
}

func TestNativeTurnRecordsWordingWarningWithoutChangingToolOutcome(t *testing.T) {
	p := &toolJourneyPlatform{}
	var evidence []map[string]any
	calls, shown := captureNativeTurn(p, "看看我的提醒", func(string) []realCanaryObservation {
		_ = p.Reply(context.Background(), nil, "No更多分页，你还有一条待处理提醒。")
		return []realCanaryObservation{{command: "reminder-list", data: map[string]any{"status": "ok"}}}
	}, &evidence)
	if shown["wording_warning"] != true || shown["tool_results_returned"] != true || len(calls) != 1 || calls[0].data["status"] != "ok" {
		t.Fatalf("wording finding must be recorded separately from actual tool success: %v", shown)
	}
}
