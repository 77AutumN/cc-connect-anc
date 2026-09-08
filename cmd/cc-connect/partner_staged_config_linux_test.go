//go:build linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/actionhost/crmfollowup"
	"github.com/chenhg5/cc-connect/config"
)

// This opt-in test parses a protected, inactive candidate with production
// validators. It creates no adapters, agents, listeners or ledger connections.
func TestPartnerStagedConfig_OfflineProductionValidators(t *testing.T) {
	path := os.Getenv("MYANC_PARTNER_STAGED_CONFIG")
	if path == "" {
		t.Skip("requires an explicitly staged root-private candidate")
	}
	if os.Geteuid() != 0 || !strings.HasPrefix(filepath.Clean(path), "/root/partner-config.") {
		t.Fatal("protected root candidate required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("candidate must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != os.Getenv("MYANC_PARTNER_STAGED_CONFIG_SHA256") {
		t.Fatal("candidate hash differs")
	}
	var cfg config.Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatal("candidate TOML decode failed; contents withheld")
	}
	if len(cfg.Projects) != 6 || cfg.Management.Enabled == nil || *cfg.Management.Enabled || cfg.Bridge.Enabled == nil || *cfg.Bridge.Enabled || cfg.Webhook.Enabled == nil || *cfg.Webhook.Enabled {
		t.Fatal("expected six environments and disabled management surfaces")
	}
	hosts := map[string]*crmfollowup.Adapter{}
	for _, project := range cfg.Projects {
		hosts[project.Name] = &crmfollowup.Adapter{}
	}
	if err := validateCRMProjectSet(&cfg, hosts); err != nil {
		t.Fatal("production route validator rejected candidate; contents withheld")
	}
	if err := validatePartnerRuntimeAccounts(&cfg, user.Lookup); err != nil {
		t.Fatal("production OS-account validator rejected candidate")
	}
}
