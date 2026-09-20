package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

// configureFileWork opens only existing protected host state. It does not
// initialize storage, install dependencies, or change an account's privileges.
func configureFileWork(project config.ProjectConfig, engine *core.Engine, platforms []core.Platform) (*core.ActionToolServer, func(), error) {
	noop := func() {}
	if !project.FileWork.Enabled {
		return nil, noop, nil
	}
	errInvalid := errors.New("file work requires a protected, explicitly provisioned isolated runtime")
	if project.RunAsUser == "" || len(platforms) != 1 || project.Mode != "" {
		return nil, noop, errInvalid
	}
	account, err := user.Lookup(project.RunAsUser)
	if err != nil {
		return nil, noop, errInvalid
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 || uid == os.Geteuid() {
		return nil, noop, errInvalid
	}
	for _, key := range []string{"file_work_launcher", "file_work_config"} {
		path, _ := project.Agent.Options[key].(string)
		if files.ValidateHostFile(path) != nil {
			return nil, noop, errInvalid
		}
	}
	cfg := project.FileWork
	runtimeConfigPath, _ := project.Agent.Options["file_work_config"].(string)
	raw, err := os.ReadFile(runtimeConfigPath)
	if err != nil || !fileRuntimeMatches(raw, cfg) {
		return nil, noop, errInvalid
	}
	for _, dir := range []string{cfg.WorkBaseDir, filepath.Dir(cfg.ToolsSocket)} {
		if files.ValidateHostDirectory(dir) != nil {
			return nil, noop, errInvalid
		}
	}
	sender, ok := platforms[0].(core.FileReceiptSender)
	if !ok {
		return nil, noop, errInvalid
	}
	store, err := files.Open(cfg.Ledger)
	if err != nil {
		return nil, noop, err
	}
	host, err := files.New(store, cfg.Snapshots, sender.SendFileWithReceipt)
	if err != nil {
		_ = store.Close()
		return nil, noop, err
	}
	closeHost := func() { _ = host.Close(); _ = store.Close() }
	if err := engine.SetFileWorkHost(host, func(sessionID string) (string, int, error) {
		root, err := files.PrepareWork(cfg.WorkBaseDir, sessionID, uid)
		return root, uid, err
	}); err != nil {
		closeHost()
		return nil, noop, err
	}
	server, err := core.ListenActionToolsUnix(cfg.ToolsSocket, engine.ActionToolHandler())
	if err != nil {
		closeHost()
		return nil, noop, err
	}
	return server, func() { _ = server.Close(); closeHost() }, nil
}

func fileRuntimeMatches(raw []byte, cfg config.FileWorkConfig) bool {
	if len(raw) > 16<<10 {
		return false
	}
	var runtimeConfig map[string]json.RawMessage
	if json.Unmarshal(raw, &runtimeConfig) != nil || len(runtimeConfig) != 4 {
		return false
	}
	var enabled bool
	var base, socket, command string
	if json.Unmarshal(runtimeConfig["enabled"], &enabled) != nil || json.Unmarshal(runtimeConfig["work_base_dir"], &base) != nil || json.Unmarshal(runtimeConfig["action_tools_socket"], &socket) != nil || json.Unmarshal(runtimeConfig["command"], &command) != nil {
		return false
	}
	// A runtime config must not accidentally mount the daemon/admin socket.
	return enabled && base == cfg.WorkBaseDir && socket == cfg.ToolsSocket && filepath.IsAbs(command)
}
