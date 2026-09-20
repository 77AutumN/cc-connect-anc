package claudecode

import (
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

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
