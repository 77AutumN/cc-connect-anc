package files

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The disposable CI initializer supplies two ordinary identities. No test body
// may run as root or retain capabilities; ordinary package runs skip explicitly.
func TestWorkHandoffOrdinaryIdentities(t *testing.T) {
	path := os.Getenv("CC_FILE_HANDOFF_FIXTURE")
	if path == "" {
		t.Skip("requires disposable Linux identity fixture")
	}
	var f struct {
		Root                 string
		GatewayUID, ModelUID int
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
}
