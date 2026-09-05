package claudecode

import (
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestClaudeChildEnvStripsHostSecretsAfterExtraMerge(t *testing.T) {
	base := []string{
		"SAFE=base",
		"CLAUDECODE=1",
		"MYANC_CRM_HOST_SECRET=parent-secret",
		"MYANC_CRM_HOST_SECRET_FILE=/run/secrets/parent",
	}
	extra := []string{
		"SAFE=extra",
		"MYANC_CRM_HOST_SECRET=project-secret",
		"MYANC_CRM_HOST_SECRET_FILE=/run/secrets/project",
		"MYANC_CRM_ACTION_TOKEN=stage-only-token",
		"MYANC_CRM_STAGE_INPUT=/workspace/.crm-input/token.json",
	}

	env := claudeChildEnv(base, extra)
	for _, key := range []string{"CLAUDECODE", "MYANC_CRM_HOST_SECRET", "MYANC_CRM_HOST_SECRET_FILE"} {
		if envContainsKey(env, key) {
			t.Fatalf("host-only environment %s reached Claude: %#v", key, env)
		}
	}
	if !envContains(env, "SAFE", "extra") || !envContains(env, "MYANC_CRM_ACTION_TOKEN", "stage-only-token") ||
		!envContains(env, "MYANC_CRM_STAGE_INPUT", "/workspace/.crm-input/token.json") {
		t.Fatalf("expected non-host environment missing: %#v", env)
	}
}

func TestPreserveCRMStageEnvironmentAddsOnlyStageInputs(t *testing.T) {
	opts := preserveCRMStageEnvironment(core.SpawnOptions{RunAsUser: "claude", EnvAllowlist: []string{"CUSTOM"}}, []string{
		"MYANC_CRM_ACTION_TOKEN=stage-only-token",
		"MYANC_CRM_STAGE_INPUT=/workspace/.crm-input/token.json",
		"MYANC_CRM_HOST_SECRET=must-never-be-preserved",
		"MYANC_CRM_HOST_SECRET_FILE=/run/secrets/must-never-be-preserved",
	})
	want := map[string]bool{"CUSTOM": true, "MYANC_CRM_ACTION_TOKEN": true, "MYANC_CRM_STAGE_INPUT": true}
	for _, key := range opts.EnvAllowlist {
		if !want[key] {
			t.Fatalf("unexpected run_as allowlist entry %q: %#v", key, opts.EnvAllowlist)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing run_as allowlist entries: %#v", want)
	}
}

func envContains(env []string, key, value string) bool {
	want := key + "=" + value
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func envContainsKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
