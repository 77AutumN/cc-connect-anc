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

func TestGroupFileRealmPinsBotAndGroup(t *testing.T) {
	p := config.ProjectConfig{FileWork: config.FileWorkConfig{Runtime: "native"}, Platforms: []config.PlatformConfig{{Type: "feishu", Options: map[string]any{"app_id": "fixture-app", "allow_chat": "fixture-group"}}}}
	want, err := fileGroupRealm(p)
	if err != nil || len(want) != 64 {
		t.Fatal("valid group rejected", err)
	}
	p.Platforms[0].Options["domain"] = "HTTPS://OPEN.FEISHU.CN/"
	if got, err := fileGroupRealm(p); err != nil || got != want {
		t.Fatal("equivalent endpoint changed realm")
	}
	for _, key := range []string{"app_id", "allow_chat", "domain"} {
		old := p.Platforms[0].Options[key]
		p.Platforms[0].Options[key] = "other-fixture"
		if got, err := fileGroupRealm(p); err != nil || got == want {
			t.Fatal("another Bot/group shared realm", key)
		}
		p.Platforms[0].Options[key] = old
	}
	p.Platforms[0].Options["allow_chat"] = "*"
	if _, err := fileGroupRealm(p); err == nil {
		t.Fatal("wildcard group accepted")
	}
	p.Platforms[0].Options["allow_chat"] = "fixture-group"
	p.FileWork.Runtime = ""
	if _, err := fileGroupRealm(p); err == nil {
		t.Fatal("non-native runtime accepted")
	}
}
