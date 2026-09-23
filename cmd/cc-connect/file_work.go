package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

// Standalone bounded stdin preflight; runs before config, logs or receivers.
func checkOfficeInput(args []string, input io.Reader) error {
	if len(args) != 1 || filepath.Base(args[0]) != args[0] {
		return files.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(input, files.MaxFileBytes+1))
	if err != nil {
		return err
	}
	return files.ValidateOfficeDocument(args[0], data)
}

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
	if project.FileWork.Runtime != "" && project.FileWork.Runtime != "native" {
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
	gid, err := strconv.Atoi(account.Gid)
	if err != nil || files.ValidateWorkGroup(project.FileWork.WorkBaseDir, gid) != nil {
		return nil, noop, errInvalid
	}
	cfg := project.FileWork
	if cfg.Runtime == "native" {
		// Native mode reuses the fixed /tool listener. Reject misleading socket
		// configuration rather than accidentally starting another endpoint.
		if cfg.ToolsSocket != "" {
			return nil, noop, errInvalid
		}
	} else {
		for _, key := range []string{"file_work_launcher", "file_work_config"} {
			path, _ := project.Agent.Options[key].(string)
			if files.ValidateHostFile(path) != nil {
				return nil, noop, errInvalid
			}
		}
		runtimeConfigPath, _ := project.Agent.Options["file_work_config"].(string)
		raw, err := os.ReadFile(runtimeConfigPath)
		if err != nil || !fileRuntimeMatches(raw, cfg) || files.ValidateHostDirectory(filepath.Dir(cfg.ToolsSocket)) != nil {
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
	if cfg.DocumentValidator != "" {
		if err := host.SetDocumentFormats(cfg.DocumentValidator); err != nil {
			closeHost()
			return nil, noop, err
		}
	}
	if cfg.GroupReferences {
		realm, err := fileGroupRealm(project)
		if err != nil || host.SetGroupReferences(realm) != nil {
			closeHost()
			return nil, noop, errInvalid
		}
	}
	if err := engine.SetFileWorkHost(host, func(sessionID string) (string, int, error) {
		root, err := files.PrepareWork(cfg.WorkBaseDir, sessionID, uid)
		return root, uid, err
	}); err != nil {
		closeHost()
		return nil, noop, err
	}
	if cfg.DocumentValidator != "" {
		receiver, ok := platforms[0].(interface{ SetFileWorkImagesEnabled(bool) error })
		if !ok || receiver.SetFileWorkImagesEnabled(true) != nil {
			closeHost()
			return nil, noop, errInvalid
		}
	}
	if cfg.Runtime == "native" {
		return nil, closeHost, nil
	}
	server, err := core.ListenActionToolsUnix(cfg.ToolsSocket, engine.ActionToolHandler())
	if err != nil {
		closeHost()
		return nil, noop, err
	}
	return server, func() { _ = server.Close(); closeHost() }, nil
}

func fileGroupRealm(project config.ProjectConfig) (string, error) {
	if project.FileWork.Runtime != "native" || len(project.Platforms) != 1 || project.Platforms[0].Type != "feishu" {
		return "", files.ErrInvalid
	}
	opts := project.Platforms[0].Options
	app, _ := opts["app_id"].(string)
	chat, _ := opts["allow_chat"].(string)
	if app == "" || chat == "" || strings.ContainsAny(chat, "*, \t\r\n") {
		return "", files.ErrInvalid
	}
	domain, _ := opts["domain"].(string)
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), "/")
	if domain == "" {
		domain = "https://open.feishu.cn"
	}
	raw, _ := json.Marshal([]string{domain, app, chat})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
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
