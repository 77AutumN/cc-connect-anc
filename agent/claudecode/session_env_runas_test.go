//go:build !windows

package claudecode

import (
	"context"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestRunAsActionTokenDoesNotEnterSudoAuditEnvironment(t *testing.T) {
	opts := preserveCRMStageEnvironment(core.SpawnOptions{RunAsUser: "claude"}, []string{
		"MYANC_CRM_ACTION_TOKEN=fixture-session-token",
	})
	cmd := core.BuildSpawnCommand(context.Background(), opts, "claude", "--version")
	for _, arg := range cmd.Args {
		// sudo logs values named in --preserve-env as ENV= in its audit log,
		// even when the values themselves are absent from argv.
		if strings.HasPrefix(arg, "--preserve-env=") && strings.Contains(arg, "MYANC_CRM_ACTION_TOKEN") {
			t.Fatal("session token would enter sudo's explicit environment audit field")
		}
	}
	if !envContains(core.FilterEnvForSpawn(claudeChildEnv(nil, []string{
		"MYANC_CRM_ACTION_TOKEN=fixture-session-token",
	}), opts), "MYANC_CRM_ACTION_TOKEN", "fixture-session-token") {
		t.Fatal("session token must remain in the filtered process environment")
	}
}

func TestRunAsFilterKeepsActionTokenAndDropsHostSecrets(t *testing.T) {
	extra := []string{
		"MYANC_CRM_ACTION_TOKEN=stage-only-token",
		"MYANC_CRM_STAGE_INPUT=/workspace/.crm-input/token.json",
		"MYANC_CRM_HOST_SECRET=host-secret",
		"MYANC_CRM_HOST_SECRET_FILE=/run/secrets/host-secret",
	}
	opts := preserveCRMStageEnvironment(core.SpawnOptions{RunAsUser: "claude"}, extra)
	env := core.FilterEnvForSpawn(claudeChildEnv(nil, extra), opts)
	if !envContains(env, "MYANC_CRM_ACTION_TOKEN", "stage-only-token") {
		t.Fatalf("stage-only action token did not cross run_as boundary: %#v", env)
	}
	if !envContains(env, "MYANC_CRM_STAGE_INPUT", "/workspace/.crm-input/token.json") {
		t.Fatalf("workspace-local stage input did not cross run_as boundary: %#v", env)
	}
	for _, key := range []string{"MYANC_CRM_HOST_SECRET", "MYANC_CRM_HOST_SECRET_FILE"} {
		if envContainsKey(env, key) {
			t.Fatalf("host capability %s crossed run_as boundary: %#v", key, env)
		}
	}
}
