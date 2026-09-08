package main

import (
	"errors"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/config"
)

// A different login name need not mean a different UID or HOME. Check the OS
// accounts before constructing agents, even when the configuration is valid.
func validatePartnerRuntimeAccounts(cfg *config.Config, lookup func(string) (*user.User, error)) error {
	uids, homes := map[string]bool{}, map[string]bool{}
	for _, project := range cfg.Projects {
		account, err := lookup(project.RunAsUser)
		if err != nil || account == nil || account.Username != project.RunAsUser || account.Uid == "" || account.Uid == "0" || uids[account.Uid] {
			return errors.New("fixed environments require verified distinct non-root runtime UIDs")
		}
		home := filepath.Clean(account.HomeDir)
		work, _ := project.Agent.Options["work_dir"].(string)
		rel, err := filepath.Rel(home, work)
		if !filepath.IsAbs(home) || homes[home] || err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("each fixed workspace must be inside its own distinct runtime HOME")
		}
		uids[account.Uid], homes[home] = true, true
	}
	return nil
}
