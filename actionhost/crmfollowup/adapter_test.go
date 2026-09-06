package crmfollowup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestNewFromEnvToolsRequiresExplicitOptInAndHost(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		secret  bool
		enabled bool
		invalid bool
	}{
		{"", false, false, false}, {"", true, false, false},
		{"1", true, true, false}, {"1", false, false, true},
		{"true", true, false, true}, {"0", true, false, true},
	} {
		t.Run(fmt.Sprintf("mode=%q_host=%t", tc.mode, tc.secret), func(t *testing.T) {
			t.Setenv(hostSecretFileEnv, "")
			t.Setenv(hostSecretEnv, "")
			if tc.secret {
				t.Setenv(hostSecretEnv, strings.Repeat("s", minHostSecretLen))
			}
			t.Setenv(toolsEnv, tc.mode)
			t.Setenv(projectEnv, "test")
			t.Setenv(commandEnv, markerCommand)
			a, _, err := NewFromEnv()
			if (err != nil) != tc.invalid {
				t.Fatalf("invalid=%t, got %v", tc.invalid, err)
			}
			if err == nil && ((a != nil && a.ToolsEnabled()) != tc.enabled) {
				t.Fatal("tools mode was not explicit")
			}
			if _, present := os.LookupEnv(hostSecretEnv); present {
				t.Fatal("host secret survived construction")
			}
		})
	}
}

func TestRestartEnvRestoresOnlySupervisorCredentialsWithoutGlobalExposure(t *testing.T) {
	for _, fileMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("file=%t", fileMode), func(t *testing.T) {
			t.Setenv(hostSecretEnv, "")
			t.Setenv(hostSecretFileEnv, "")
			t.Setenv(toolsEnv, "1")
			t.Setenv(projectEnv, "test")
			t.Setenv(commandEnv, markerCommand)
			secret := strings.Repeat("s", minHostSecretLen)
			if fileMode {
				path := filepath.Join(t.TempDir(), "host-secret")
				if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv(hostSecretFileEnv, path)
			} else {
				t.Setenv(hostSecretEnv, secret)
			}
			a, _, err := NewFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			base := append(os.Environ(), actionTokenEnv+"=stale", stageInputEnv+"=/stale", hostSecretEnv+"=stale")
			restartEnv := a.RestartEnv(base)
			if os.Getenv(hostSecretEnv) != "" || os.Getenv(hostSecretFileEnv) != "" {
				t.Fatal("restart exposed a global secret")
			}
			if envKeyPresent(restartEnv, actionTokenEnv) || envKeyPresent(restartEnv, stageInputEnv) {
				t.Fatal("restarted with stale session material")
			}
			if envKeyPresent(restartEnv, hostSecretEnv) == fileMode {
				t.Fatal("file mode must not export the secret value")
			}
			if !envHas(base, hostSecretEnv, "stale") {
				t.Fatal("mutated caller environment slice")
			}
			// Simulate only the new supervisor's environment, then its first action.
			for _, item := range restartEnv {
				key, value, _ := strings.Cut(item, "=")
				if key == hostSecretEnv || key == hostSecretFileEnv {
					t.Setenv(key, value)
				}
			}
			next, project, err := NewFromEnv()
			if err != nil || next == nil || !next.ToolsEnabled() || next.hostSecret != secret || project != "test" {
				t.Fatal("restart lost the configured tools host")
			}
			if os.Getenv(hostSecretEnv) != "" || os.Getenv(hostSecretFileEnv) != "" {
				t.Fatal("new supervisor did not clear credentials")
			}
		})
	}
}

func TestBeginUsesHostProtocolAndKeepsSecretOutOfPayload(t *testing.T) {
	t.Setenv(hostSecretFileEnv, "/run/secrets/must-not-reach-helper")
	var gotSubcommand string
	var gotPayload []byte
	var gotEnv []string
	a := &Adapter{command: "/usr/local/bin/crm-followup", hostSecret: "host-secret", run: func(_ context.Context, command, subcommand string, payload []byte, env []string) ([]byte, error) {
		if command != "/usr/local/bin/crm-followup" {
			t.Fatalf("command = %q", command)
		}
		gotSubcommand, gotPayload, gotEnv = subcommand, payload, env
		return []byte(`{"status":"pending","approval_id":"apr_1","change_id":"chg_Abcdefgh12345678","expires_at":"2026-09-04T00:10:00+08:00","preview":{"actor":"Example Operator","customer":{"customer_number":"ANC-0001","name":"<CRM_DATA>Example Customer</CRM_DATA>"},"effects":{"followup":{"after":{"occurred_at":"2026-09-04T00:00:00+08:00","content":"<CRM_DATA>Test follow-up</CRM_DATA>"}}}},"superseded_approval_ids":["apr_old"]}`), nil
	}}
	token := strings.Repeat("t", 32)
	configureTestWorkDir(t, a)
	if err := os.WriteFile(a.stageInputPath(token), []byte(`{"customer_query":"ANC-0001","content":"test"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	principal := core.ActionPrincipal{Platform: "feishu", UserID: "owner", ChatID: "chat", SessionKey: "feishu:chat:owner", Project: "crm", MessageID: "msg"}
	result, err := a.Begin(context.Background(), core.ActionRef{Kind: Kind}, principal, token, core.LangChinese)
	if err != nil {
		t.Fatal(err)
	}
	if gotSubcommand != "host-prepare" || result.Status != "pending" || result.ApprovalID != "apr_1" {
		t.Fatalf("protocol result = %#v, subcommand=%q", result, gotSubcommand)
	}
	if strings.Contains(string(gotPayload), "host-secret") {
		t.Fatal("host secret leaked into JSON payload")
	}
	var request map[string]any
	if err := json.Unmarshal(gotPayload, &request); err != nil {
		t.Fatal(err)
	}
	stageRequest, ok := request["request"].(map[string]any)
	if !ok || stageRequest["customer_query"] != "ANC-0001" || request["principal"] == nil {
		t.Fatalf("request = %#v", request)
	}
	if !envHas(gotEnv, hostSecretEnv, "host-secret") || !envHas(gotEnv, actionTokenEnv, token) {
		t.Fatal("helper did not receive both host-only and session credentials")
	}
	if envKeyPresent(gotEnv, hostSecretFileEnv) {
		t.Fatal("secret file path leaked into helper environment")
	}
	if result.Card == nil || len(result.Card.Elements) == 0 {
		t.Fatal("pending approval did not produce a business card")
	}
	if _, err := os.Stat(a.stageInputPath(token)); !os.IsNotExist(err) {
		t.Fatalf("consumed stage input still exists: %v", err)
	}
}

func TestClaimPayloadContainsOnlyApprovalDecisionAndPrincipal(t *testing.T) {
	var gotPayload []byte
	a := &Adapter{command: "crm-followup", hostSecret: "secret", run: func(_ context.Context, _, subcommand string, payload []byte, _ []string) ([]byte, error) {
		if subcommand != "host-claim" {
			t.Fatalf("subcommand = %q", subcommand)
		}
		gotPayload = payload
		return []byte(`{"status":"cancelled","approval_id":"apr_1","change_id":"chg_Abcdefgh12345678","message":"Feishu was not changed."}`), nil
	}}
	principal := core.ActionPrincipal{Platform: "feishu", UserID: "owner", ChatID: "chat", SessionKey: "feishu:chat:owner", Project: "crm", MessageID: "card"}
	result, execute, err := a.Claim(context.Background(), "apr_1", core.ActionCancel, principal, core.LangEnglish)
	if err != nil {
		t.Fatal(err)
	}
	if execute {
		t.Fatal("cancel decision unexpectedly requested execution")
	}
	var request map[string]any
	if err := json.Unmarshal(gotPayload, &request); err != nil {
		t.Fatal(err)
	}
	if len(request) != 3 || request["approval_id"] != "apr_1" || request["decision"] != "cancel" {
		t.Fatalf("unexpected host-claim payload: %#v", request)
	}
	if result.Status != "cancelled" || result.Card == nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestClaimDoesNotReturnStaleExecutingCardForDuplicateCallback(t *testing.T) {
	a := &Adapter{command: "crm-followup", hostSecret: "secret", run: func(_ context.Context, _, subcommand string, _ []byte, _ []string) ([]byte, error) {
		if subcommand != "host-claim" {
			t.Fatalf("subcommand = %q", subcommand)
		}
		return []byte(`{"status":"executing","code":"apply_in_progress","approval_id":"apr_1","change_id":"chg_Abcdefgh12345678","execute":false}`), nil
	}}
	principal := core.ActionPrincipal{Platform: "feishu", UserID: "owner", ChatID: "chat", SessionKey: "feishu:chat:owner", Project: "crm", MessageID: "card"}
	result, execute, err := a.Claim(context.Background(), "apr_1", core.ActionApprove, principal, core.LangEnglish)
	if err != nil {
		t.Fatal(err)
	}
	if execute || result.Card != nil || result.Status != "executing" {
		t.Fatalf("duplicate claim = (%#v, %v), want toast-only executing result", result, execute)
	}
}

func TestClaimExecutingCardPreservesFrozenPreviewAndRemovesButtons(t *testing.T) {
	a := &Adapter{command: "fixture", run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		return []byte(`{"status":"executing","execute":true,"preview":{"actor":"Fixture Owner","customer":{"customer_number":"C-001","name":"Frozen Company"},"effects":{"followup":{"after":{"occurred_at":"2026-09-06T14:00:00+08:00","content":"Frozen followup"}}}}}`), nil
	}}
	result, execute, err := a.Claim(context.Background(), "apr_fixture", core.ActionApprove, core.ActionPrincipal{}, core.LangChinese)
	if err != nil || !execute || result.Card == nil {
		t.Fatalf("claim failed: execute=%v err=%v", execute, err)
	}
	visible := result.Card.RenderText()
	for _, want := range []string{"Fixture Owner", "Frozen Company", "Frozen followup", "C-001"} {
		if !strings.Contains(visible, want) {
			t.Errorf("executing preview lost %q", want)
		}
	}
	if result.Card.HasButtons() {
		t.Fatal("executing card is still actionable")
	}
}

func TestBindCardRequiresHelperToBindTheExactEmittedMessage(t *testing.T) {
	var gotPayload []byte
	a := &Adapter{command: "crm-followup", hostSecret: "secret", run: func(_ context.Context, _, subcommand string, payload []byte, _ []string) ([]byte, error) {
		if subcommand != "host-bind-card" {
			t.Fatalf("subcommand = %q", subcommand)
		}
		gotPayload = payload
		return []byte(`{"status":"bound","approval_id":"apr_1","change_id":"chg_1"}`), nil
	}}
	principal := core.ActionPrincipal{Platform: "feishu", UserID: "owner", ChatID: "chat", SessionKey: "feishu:chat:owner", Project: "crm", MessageID: "om_emitted_card"}
	if err := a.BindCard(context.Background(), "apr_1", principal); err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(gotPayload, &request); err != nil {
		t.Fatal(err)
	}
	boundPrincipal, _ := request["principal"].(map[string]any)
	if len(request) != 2 || request["approval_id"] != "apr_1" || boundPrincipal["message_id"] != "om_emitted_card" {
		t.Fatalf("unexpected host-bind-card payload: %#v", request)
	}

	a.run = func(context.Context, string, string, []byte, []string) ([]byte, error) {
		return []byte(`{"status":"blocked","code":"card_mismatch"}`), nil
	}
	if err := a.BindCard(context.Background(), "apr_1", principal); err == nil {
		t.Fatal("helper refusal was treated as a successful card binding")
	}
}

func TestNewFromEnvReadsSecretFileAndUnsetsSupervisorSecrets(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "host-secret")
	wantSecret := strings.Repeat("s", minHostSecretLen)
	if err := os.WriteFile(secretPath, []byte(wantSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hostSecretEnv, "")
	t.Setenv(hostSecretFileEnv, secretPath)
	t.Setenv(actionTokenEnv, "must-not-survive")
	t.Setenv(stageInputEnv, "/must/not/survive")
	t.Setenv(projectEnv, "claude-local")
	t.Setenv(commandEnv, "/usr/local/bin/crm-followup")
	a, project, err := NewFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || a.hostSecret != wantSecret || project != "claude-local" {
		t.Fatalf("adapter=%#v project=%q", a, project)
	}
	for _, key := range []string{hostSecretEnv, hostSecretFileEnv, actionTokenEnv, stageInputEnv} {
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s remained in supervisor environment", key)
		}
	}
}

func TestNewFromEnvFailsClosedOnWeakSecretOrRelativeCommand(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		command string
	}{
		{name: "short secret", secret: strings.Repeat("s", minHostSecretLen-1), command: "/usr/local/bin/crm-followup"},
		{name: "long secret", secret: strings.Repeat("s", maxHostSecretLen+1), command: "/usr/local/bin/crm-followup"},
		{name: "relative helper", secret: strings.Repeat("s", minHostSecretLen), command: "crm-followup"},
		{name: "missing helper", secret: strings.Repeat("s", minHostSecretLen)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hostSecretEnv, tc.secret)
			t.Setenv(hostSecretFileEnv, "")
			t.Setenv(projectEnv, "claude-local")
			t.Setenv(commandEnv, tc.command)
			if adapter, _, err := NewFromEnv(); err == nil || adapter != nil {
				t.Fatalf("NewFromEnv() = (%#v, %v), want fail-closed", adapter, err)
			}
		})
	}
}

func TestNewFromEnvRejectsEmptySecretFileWithoutLeakingPath(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "private-name-must-not-leak")
	if err := os.WriteFile(secretPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hostSecretEnv, "")
	t.Setenv(hostSecretFileEnv, secretPath)
	t.Setenv(projectEnv, "claude-local")
	t.Setenv(commandEnv, "/usr/local/bin/crm-followup")
	_, _, err := NewFromEnv()
	if err == nil {
		t.Fatal("empty secret file was accepted")
	}
	if strings.Contains(err.Error(), secretPath) {
		t.Fatalf("secret path leaked in error: %v", err)
	}
}

func TestClaimRejectsUnknownDecisionBeforeRunningHelper(t *testing.T) {
	a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		t.Fatal("helper was called for an unknown decision")
		return nil, nil
	}}
	if _, _, err := a.Claim(context.Background(), "apr_1", core.ActionDecision("other"), core.ActionPrincipal{}, core.LangEnglish); err == nil {
		t.Fatal("unknown decision was accepted")
	}
}

func TestBeginFailsClosedWithoutSessionRequest(t *testing.T) {
	a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		t.Fatal("helper was called without a session request")
		return nil, nil
	}}
	configureTestWorkDir(t, a)
	if _, err := a.Begin(context.Background(), core.ActionRef{Kind: Kind}, core.ActionPrincipal{}, "missing-request-token", core.LangEnglish); err == nil {
		t.Fatal("missing session request was accepted")
	}
}

func TestBeginRejectsStageInputOver64KiB(t *testing.T) {
	a := &Adapter{run: func(context.Context, string, string, []byte, []string) ([]byte, error) {
		t.Fatal("helper was called for an oversized stage request")
		return nil, nil
	}}
	configureTestWorkDir(t, a)
	token := "oversized-request-token"
	request := make([]byte, maxStageInputSize+1)
	if err := os.WriteFile(a.stageInputPath(token), request, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Begin(context.Background(), core.ActionRef{Kind: Kind}, core.ActionPrincipal{}, token, core.LangEnglish); err == nil {
		t.Fatal("stage input larger than 64 KiB was accepted")
	}
}

func TestApprovalCardCarriesOnlyOpaqueAuthorizationReference(t *testing.T) {
	card := approvalCard(map[string]any{
		"status": "pending", "approval_id": "apr_1", "change_id": "chg_1",
		"preview": map[string]any{"actor": "operator"},
	}, core.LangEnglish)
	var buttons []core.CardButton
	for _, element := range card.Elements {
		if actions, ok := element.(core.CardActions); ok {
			buttons = append(buttons, actions.Buttons...)
		}
	}
	if len(buttons) != 3 {
		t.Fatalf("buttons = %#v", buttons)
	}
	for _, button := range buttons {
		if button.Value != "" || len(button.Extra) != 4 || button.Extra["kind"] != Kind || button.Extra["approval_id"] != "apr_1" || button.Extra["language"] != "en" {
			t.Fatalf("button leaks non-reference data: %#v", button)
		}
		if _, ok := button.Extra["change_id"]; ok {
			t.Fatalf("change id must not be card authority: %#v", button.Extra)
		}
	}
}

func TestApprovalCardDescribesRepairWithoutPromisingNewFollowup(t *testing.T) {
	card := approvalCard(map[string]any{
		"status": "pending", "approval_id": "apr_1",
		"preview": map[string]any{
			"expires_at": "2026-09-04T00:10:00+08:00",
			"effects": map[string]any{
				"followup": map[string]any{
					"action": "already_verified",
					"after":  map[string]any{"content": "existing follow-up"},
				},
			},
			"execution_order": []any{"read_followup", "read_customer", "update_customer_summary", "read_customer"},
		},
	}, core.LangEnglish)
	visible := card.RenderText()
	for _, want := range []string{"Existing verified follow-up (no new row)", "repair and read back the customer summary"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("repair preview missing %q: %s", want, visible)
		}
	}
	for _, forbidden := range []string{"Follow-up to create", "create and read back the follow-up"} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("repair preview promises a new follow-up (%q): %s", forbidden, visible)
		}
	}
}

func TestStageOutcomesRenderNoWriteCardsWithoutApprovalButtons(t *testing.T) {
	tests := []struct {
		name      string
		result    map[string]any
		visible   []string
		forbidden []string
	}{
		{
			name: "missing time",
			result: map[string]any{
				"status": "needs_time", "code": "followup_time_required", "approval_id": "apr_hidden",
			},
			visible:   []string{"Follow-up time needed", "date, time, and time zone", "Feishu was not changed"},
			forbidden: []string{"apr_hidden", "followup_time_required"},
		},
		{
			name: "ambiguous customer",
			result: map[string]any{
				"status": "needs_selection", "approval_id": "apr_hidden",
				"candidates": []any{
					map[string]any{"customer_number": "ANC-0001", "name": "<CRM_DATA>Example Customer</CRM_DATA>", "contact": "Contact A", "record_id": "rec_hidden"},
					map[string]any{"customer_number": "ANC-0002", "name": "Example Customer", "owner": "Owner B"},
				},
			},
			visible:   []string{"Choose the customer", "Candidate 1", "ANC-0001", "Candidate 2", "ANC-0002"},
			forbidden: []string{"apr_hidden", "rec_hidden"},
		},
		{
			name: "not found",
			result: map[string]any{
				"status": "not_found", "code": "customer_not_found", "customer_query": "<CRM_DATA>Missing Customer</CRM_DATA>",
			},
			visible:   []string{"Customer not found", "Missing Customer", "Feishu was not changed"},
			forbidden: []string{"customer_not_found", "<CRM_DATA>"},
		},
		{
			name: "blocked",
			result: map[string]any{
				"status": "blocked", "code": "backend_read_failed", "message": "internal detail must not be relayed",
			},
			visible:   []string{"Request blocked", "CRM validation", "Feishu was not changed"},
			forbidden: []string{"backend_read_failed", "internal detail"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			card := approvalCard(tc.result, core.LangEnglish)
			text := card.RenderText()
			for _, want := range tc.visible {
				if !strings.Contains(text, want) {
					t.Fatalf("outcome card missing %q: %s", want, text)
				}
			}
			for _, forbidden := range tc.forbidden {
				if strings.Contains(text, forbidden) {
					t.Fatalf("outcome card leaked %q: %s", forbidden, text)
				}
			}
			for _, element := range card.Elements {
				if _, ok := element.(core.CardActions); ok {
					t.Fatalf("non-pending outcome rendered approval buttons: %#v", card)
				}
			}
		})
	}
}

func TestMatchRecognizesOnlyExactMarkerArgv(t *testing.T) {
	a := &Adapter{}
	valid := core.Event{
		Type: core.EventPermissionRequest, ToolName: "Bash",
		ToolInputRaw: map[string]any{"command": markerCommand + " request-approval"},
	}
	ref, ok := a.Match(valid)
	if !ok || ref.Kind != Kind || ref.ChangeID != "" {
		t.Fatalf("valid marker = (%#v, %v)", ref, ok)
	}
	for name, command := range map[string]string{
		"path spoof":     "/tmp/crm-followup request-approval",
		"bare command":   "crm-followup request-approval",
		"extra command":  markerCommand + " request-approval; touch /tmp/x",
		"substitution":   markerCommand + " request-approval $(echo anything)",
		"extra flag":     markerCommand + " request-approval --yes",
		"trailing space": markerCommand + " request-approval ",
	} {
		t.Run(name, func(t *testing.T) {
			event := valid
			event.ToolInputRaw = map[string]any{"command": command}
			if ref, ok := a.Match(event); ok {
				t.Fatalf("unsafe marker accepted: %#v", ref)
			}
		})
	}
}

func TestSessionEnvBindsWorkspaceInputToToken(t *testing.T) {
	workDir := t.TempDir()
	a := &Adapter{}
	configureTestWorkDirAt(t, a, workDir)
	first, err := a.SessionEnv("opaque-token-a")
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := a.SessionEnv("opaque-token-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.SessionEnv("opaque-token-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0] != actionTokenEnv+"=opaque-token-a" || first[1] != repeat[1] || first[1] == second[1] {
		t.Fatalf("session env is not stable/unique: first=%#v repeat=%#v second=%#v", first, repeat, second)
	}
	prefix := stageInputEnv + "=" + filepath.Join(workDir, ".crm-input") + string(filepath.Separator)
	if !strings.HasPrefix(first[1], prefix) || !strings.HasSuffix(first[1], ".json") {
		t.Fatalf("stage input escaped workspace: %q", first[1])
	}
	inputDir := filepath.Join(workDir, ".crm-input")
	info, err := os.Stat(inputDir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o730 {
			t.Fatalf("input directory mode = %o, want 730", got)
		}
		if info.Mode()&os.ModeSetgid == 0 || info.Mode()&os.ModeSticky == 0 {
			t.Fatalf("input directory special mode = %v, want setgid+sticky", info.Mode())
		}
	}
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("SessionEnv created request files: %#v", entries)
	}
}

func TestSetWorkDirRequiresRunAsUser(t *testing.T) {
	if err := (&Adapter{}).SetWorkDir(t.TempDir(), ""); err == nil {
		t.Fatal("empty run_as_user was accepted")
	}
}

func TestSetWorkDirDoesNotCreateSupervisorOwnedExchangeDirectory(t *testing.T) {
	workDir := t.TempDir()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	inputDir := filepath.Join(workDir, ".crm-input")
	if err := (&Adapter{}).SetWorkDir(workDir, account.Username); err == nil {
		t.Fatal("missing pre-provisioned exchange directory was accepted")
	}
	if _, err := os.Stat(inputDir); !os.IsNotExist(err) {
		t.Fatalf("SetWorkDir created the exchange directory: %v", err)
	}
}

func configureTestWorkDir(t *testing.T, a *Adapter) string {
	t.Helper()
	workDir := t.TempDir()
	configureTestWorkDirAt(t, a, workDir)
	return workDir
}

func configureTestWorkDirAt(t *testing.T, a *Adapter, workDir string) {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	inputDir := filepath.Join(workDir, ".crm-input")
	if err := os.Mkdir(inputDir, 0o730); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(inputDir, os.FileMode(0o730)|os.ModeSetgid|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SetWorkDir(workDir, account.Username); err != nil {
		t.Fatal(err)
	}
}

func envHas(env []string, key, want string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix) == want
		}
	}
	return false
}

func envKeyPresent(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
