package files

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// The disposable CI initializer supplies two ordinary identities. No test body
// may run as root or retain capabilities; ordinary package runs skip explicitly.
func TestWorkHandoffOrdinaryIdentities(t *testing.T) {
	path := os.Getenv("CC_FILE_HANDOFF_FIXTURE")
	if path == "" {
		t.Skip("requires disposable Linux identity fixture")
	}
	var f struct {
		Root                                    string
		GatewayUID, ModelUID                    int
		Model, Command, Launcher, Config, Probe string
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 || os.Geteuid() != f.GatewayUID || f.ModelUID <= 0 || f.ModelUID == f.GatewayUID {
		t.Fatal("ordinary distinct identities required")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "CapEff:\t0000000000000000") {
		t.Fatal("gateway retained privileges")
	}
	// The old 0700 model-owned handoff cannot be read by the ordinary host.
	if _, err := os.ReadFile(filepath.Join(f.Root, "legacy-private", "result.docx")); !os.IsPermission(err) {
		t.Fatal("host bypassed model-private permissions", err)
	}
	t.Logf("ordinary gateway uid=%d, model uid=%d; no effective capabilities; legacy private output denied", f.GatewayUID, f.ModelUID)
	if _, err := PrepareWork(filepath.Join(f.Root, "works"), "ordinary-session", f.ModelUID); err != nil {
		t.Fatalf("ordinary gateway cannot prepare model work: %v", err)
	}
	probe, err := os.ReadFile(f.Probe)
	if err != nil {
		t.Fatal(err)
	}
	runFileJourney(t, filepath.Join(f.Root, "works"), f.ModelUID, func(shared *fileJourneyShared, fixture fixture, engine *core.Engine) {
		server, err := core.ListenActionToolsUnix(filepath.Join(f.Root, "tools.sock"), engine.ActionToolHandler())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		go func() { _ = server.Serve() }()
		adminPath := filepath.Join(f.Root, "admin.sock")
		admin, err := net.Listen("unix", adminPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		other, err := PrepareWork(filepath.Join(f.Root, "works"), "other-private-work", f.ModelUID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, "inputs", "other.txt"), []byte("fictional-other-work"), 0440); err != nil {
			t.Fatal(err)
		}
		shared.external = func(a *fileJourneyAgent, step fileJourneyStep) (result fileJourneyResult) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sudo", "-n", "-u", f.Model, "--", "/usr/bin/python3", "-I", "-B", f.Launcher, "--config", f.Config, "--work-root", a.root, "--", f.Command, "-c", string(probe))
			cmd.Env = append(os.Environ(), "CC_FILE_ACTION_TOKEN="+a.token)
			payload, _ := json.Marshal(map[string]any{"input": step.inputName, "from": step.from, "to": step.to, "extension": step.extension, "reference": step.reference, "model_uid": f.ModelUID, "hidden": []string{other, fixture.database, fixture.snapshots, adminPath, f.Config}})
			cmd.Stdin = bytes.NewReader(payload)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				result.err = fmt.Errorf("ordinary model probe: %w: %s", err, stderr.String())
				return
			}
			var output struct {
				Work     core.FileWorkContext
				Artifact core.FileWorkArtifact
				Checks   []string
			}
			if result.err = json.Unmarshal(stdout.Bytes(), &output); result.err != nil {
				return
			}
			result.work, result.artifact = output.Work, output.Artifact
			path := filepath.Join(result.work.OutputDir, result.artifact.Path)
			info, err := os.Stat(path)
			if err != nil || checkOwner(info, f.ModelUID, true) != nil || info.Mode().Perm() != 0640 {
				result.err = fmt.Errorf("artifact was not produced by ordinary model UID")
				return
			}
			result.data, result.err = os.ReadFile(path)
			t.Logf("ordinary model=%d; host=%d; artifact version=%d; checks=%v", f.ModelUID, f.GatewayUID, result.artifact.Version, output.Checks)
			return
		}
	})
}
