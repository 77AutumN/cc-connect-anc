package claudecode

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestNativeFileWorkPreservesEmployeeRuntimeAndDoesNotCopySession(t *testing.T) {
	a := &Agent{fileWorkNative: true, cmd: "/opt/runtime/claude", workDir: "/home/fixture/work",
		spawnOpts: core.SpawnOptions{RunAsUser: "fixture"}, activeIdx: -1,
		configEnv: []string{"EXISTING_SETTING=fixture"}, sessionEnv: []string{"OLD_SESSION=must-not-copy"},
		pluginDirs: []string{"/opt/fixture/plugin"}, platformPrompt: "fixture-format", ccDataDir: "/protected/data"}
	clone, err := a.ForFileWork("/srv/works/current")
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("unsupported OS accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	child := clone.(*Agent)
	if child.cmd != a.cmd || child.workDir != a.workDir || child.spawnOpts.RunAsUser != "fixture" ||
		!reflect.DeepEqual(child.cliExtraArgs, []string{"--add-dir", "/srv/works/current"}) ||
		!reflect.DeepEqual(child.pluginDirs, a.pluginDirs) || child.ccDataDir != a.ccDataDir ||
		child.platformPrompt != a.platformPrompt || len(child.sessionEnv) != 0 {
		t.Fatal("employee runtime or current work was not preserved")
	}
	if !strings.Contains(strings.Join(child.configEnv, "\n"), "CC_FILE_WORK_ROOT=/srv/works/current") ||
		!strings.Contains(strings.Join(child.configEnv, "\n"), "EXISTING_SETTING=fixture") {
		t.Fatal("native transport context missing")
	}
	if len(a.cliExtraArgs) != 0 || len(a.configEnv) != 1 || len(a.spawnOpts.EnvAllowlist) != 0 {
		t.Fatal("ordinary agent mutated")
	}
}

func TestFileWorkRuntimeNeverFallsBackToOrdinaryCLI(t *testing.T) {
	a := &Agent{cmd: "/opt/runtime/claude", spawnOpts: core.SpawnOptions{RunAsUser: "fixture"}}
	if _, err := a.ForFileWork("/srv/works/fixture"); err == nil {
		t.Fatal("missing launcher silently used normal process")
	}
	a.fileWorkLauncher = "/opt/runtime/work_sandbox.py"
	a.fileWorkConfig = "/opt/runtime/work.json"
	clone, err := a.ForFileWork("/srv/works/fixture")
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("unsupported OS accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	child := clone.(*Agent)
	if child.cmd != "/usr/bin/python3" || len(child.cliExtraArgs) != 7 || child.cliExtraArgs[4] != "/srv/works/fixture" || child.cliExtraArgs[6] != a.cmd {
		t.Fatalf("launcher bypass: %#v", child.cliExtraArgs)
	}
	if strings.Contains(fileWorkPrompt, "cron add") || !strings.Contains(fileWorkPrompt, "unknown") {
		t.Fatal("file prompt retained administrative workflow or lost outcome boundary")
	}
}
