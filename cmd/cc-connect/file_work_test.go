package main

import (
	"encoding/json"
	"github.com/chenhg5/cc-connect/config"
	"path/filepath"
	"testing"
)

func TestFileWorkConfigurationCannotMountAnotherSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := config.FileWorkConfig{WorkBaseDir: filepath.Join(dir, "works"), ToolsSocket: filepath.Join(dir, "tools.sock")}
	values := map[string]any{"enabled": true, "work_base_dir": cfg.WorkBaseDir, "action_tools_socket": cfg.ToolsSocket, "command": filepath.Join(dir, "fictional-cli")}
	encode := func() []byte {
		b, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !fileRuntimeMatches(encode(), cfg) {
		t.Fatal("matching runtime rejected")
	}
	values["action_tools_socket"] = filepath.Join(dir, "api.sock")
	if fileRuntimeMatches(encode(), cfg) {
		t.Fatal("admin socket mismatch accepted")
	}
	values["action_tools_socket"] = cfg.ToolsSocket
	values["work_base_dir"] = filepath.Join(dir, "other-work")
	if fileRuntimeMatches(encode(), cfg) {
		t.Fatal("another work base accepted")
	}
	if server, closeHost, err := configureFileWork(config.ProjectConfig{}, nil, nil); err != nil || server != nil || closeHost == nil {
		t.Fatal("default-off configuration changed startup")
	}
}
