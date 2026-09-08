package main

import (
	"fmt"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/actionhost/crmfollowup"
	"github.com/chenhg5/cc-connect/config"
)

func partnerConfigFixture(t *testing.T) (*config.Config, map[string]*crmfollowup.Adapter) {
	return partnerConfigFixtureForActors(t, 2)
}

func partnerConfigFixtureForActors(t *testing.T, count int) (*config.Config, map[string]*crmfollowup.Adapter) {
	t.Helper()
	cfg := &config.Config{}
	hosts := map[string]*crmfollowup.Adapter{}
	root := t.TempDir()
	routes := [][2]string{}
	for _, actor := range []string{"owner", "partner", "test", "unapproved"}[:count] {
		routes = append(routes, [2]string{actor, "private-" + actor}, [2]string{actor, "group"})
	}
	capacity := int64(128)
	if count > 2 {
		capacity = 85
	}
	for i, route := range routes {
		name := fmt.Sprintf("environment-%d", i)
		hosts[name] = &crmfollowup.Adapter{}
		cfg.Projects = append(cfg.Projects, config.ProjectConfig{
			Name: name, RunAsUser: name,
			Agent: config.AgentConfig{Type: "claudecode", Options: map[string]any{
				"work_dir": filepath.Join(root, name, "ClaudeRemote"), "image_cache_capacity_mib": capacity,
			}},
			Platforms: []config.PlatformConfig{{Type: "feishu", Options: map[string]any{
				"app_id": "fictional-app", "app_secret": "fixture-only",
				"allow_from": route[0], "allow_chat": route[1],
				"enable_feishu_card": true, "share_session_in_channel": false, "thread_isolation": false,
			}}},
		})
	}
	return cfg, hosts
}

func TestValidateCRMProjectSetThreePeopleSixEnvironments(t *testing.T) {
	cfg, hosts := partnerConfigFixtureForActors(t, 3)
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal("three people with separate private/group environments must be accepted:", err)
	}
}

func TestValidateCRMProjectSetThreePeopleRejectsUnsafeTopologyAndCapacity(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"two pairwise shared chats": func(c *config.Config) {
			c.Projects[5].Platforms[0].Options["allow_chat"] = "private-owner"
		},
		"missing third private route": func(c *config.Config) { c.Projects = c.Projects[:5] },
		"third actor reuses UID name": func(c *config.Config) { c.Projects[4].RunAsUser = c.Projects[0].RunAsUser },
		"third actor reuses HOME workspace": func(c *config.Config) {
			c.Projects[4].Agent.Options["work_dir"] = c.Projects[0].Agent.Options["work_dir"]
		},
		"six caches exceed total": func(c *config.Config) { c.Projects[0].Agent.Options["image_cache_capacity_mib"] = 88 },
		"zero cache":              func(c *config.Config) { c.Projects[5].Agent.Options["image_cache_capacity_mib"] = 0 },
		"negative cache":          func(c *config.Config) { c.Projects[5].Agent.Options["image_cache_capacity_mib"] = int64(-1) },
		"fractional cache":        func(c *config.Config) { c.Projects[5].Agent.Options["image_cache_capacity_mib"] = 85.5 },
		"string cache":            func(c *config.Config) { c.Projects[5].Agent.Options["image_cache_capacity_mib"] = "85" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg, hosts := partnerConfigFixtureForActors(t, 3)
			mutate(cfg)
			if err := validateCRMProjectSet(cfg, hosts); err == nil {
				t.Fatal("unsafe three-person configuration accepted")
			}
		})
	}
	cfg, hosts := partnerConfigFixtureForActors(t, 3)
	cfg.Projects[0].Agent.Options["image_cache_capacity_mib"] = 87 // Exactly 512 MiB, including int config values.
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal("bounded capacity rejected:", err)
	}
	fourPeople, fourHosts := partnerConfigFixtureForActors(t, 4)
	for i := range fourPeople.Projects {
		fourPeople.Projects[i].Agent.Options["image_cache_capacity_mib"] = 64
	}
	if err := validateCRMProjectSet(fourPeople, fourHosts); err == nil {
		t.Fatal("unapproved fourth person accepted")
	}
}

func TestPartnerRuntimeAccountsRejectAliasesAndSharedHomes(t *testing.T) {
	cfg, _ := partnerConfigFixture(t)
	accounts := map[string]*user.User{}
	for n, project := range cfg.Projects {
		accounts[project.RunAsUser] = &user.User{Username: project.RunAsUser, Uid: fmt.Sprint(61000 + n), HomeDir: filepath.Dir(project.Agent.Options["work_dir"].(string))}
	}
	lookup := func(name string) (*user.User, error) { return accounts[name], nil }
	if err := validatePartnerRuntimeAccounts(cfg, lookup); err != nil {
		t.Fatal(err)
	}
	second := accounts[cfg.Projects[1].RunAsUser]
	for name, mutate := range map[string]func(*user.User){
		"UID alias":      func(u *user.User) { u.Uid = "61000" },
		"root":           func(u *user.User) { u.Uid = "0" },
		"shared HOME":    func(u *user.User) { u.HomeDir = accounts[cfg.Projects[0].RunAsUser].HomeDir },
		"missing HOME":   func(u *user.User) { u.HomeDir = "" },
		"wrong identity": func(u *user.User) { u.Username = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *second
			mutate(&copy)
			accounts[cfg.Projects[1].RunAsUser] = &copy
			if err := validatePartnerRuntimeAccounts(cfg, lookup); err == nil {
				t.Fatal("unsafe runtime identity accepted")
			}
			accounts[cfg.Projects[1].RunAsUser] = second
		})
	}
}

func TestValidateCRMProjectSet(t *testing.T) {
	cfg, hosts := partnerConfigFixture(t)
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal(err)
	}
	cfg.Projects[0].Platforms[0].Options["domain"] = "https://open.feishu.cn/"
	if err := validateCRMProjectSet(cfg, hosts); err != nil {
		t.Fatal("equivalent domain rejected", err)
	}
	for name, mutate := range map[string]func(*config.Config){
		"duplicate user": func(c *config.Config) { c.Projects[1].RunAsUser = c.Projects[0].RunAsUser },
		"duplicate directory": func(c *config.Config) {
			c.Projects[1].Agent.Options["work_dir"] = c.Projects[0].Agent.Options["work_dir"]
		},
		"nested directory": func(c *config.Config) {
			c.Projects[1].Agent.Options["work_dir"] = filepath.Join(c.Projects[0].Agent.Options["work_dir"].(string), "child")
		},
		"missing project":      func(c *config.Config) { c.Projects = c.Projects[:3] },
		"unregistered project": func(c *config.Config) { c.Projects[0].Name = "other" },
		"duplicate route":      func(c *config.Config) { c.Projects[1].Platforms[0].Options["allow_chat"] = "private-owner" },
		"third sender":         func(c *config.Config) { c.Projects[0].Platforms[0].Options["allow_from"] = "alt" },
		"second shared chat":   func(c *config.Config) { c.Projects[2].Platforms[0].Options["allow_chat"] = "private-owner" },
		"mixed app":            func(c *config.Config) { c.Projects[0].Platforms[0].Options["app_id"] = "other" },
		"mixed domain":         func(c *config.Config) { c.Projects[0].Platforms[0].Options["domain"] = "https://open.larksuite.com" },
		"mixed credential":     func(c *config.Config) { c.Projects[0].Platforms[0].Options["app_secret"] = "different" },
		"wildcard":             func(c *config.Config) { c.Projects[0].Platforms[0].Options["allow_from"] = "*" },
		"missing capacity":     func(c *config.Config) { delete(c.Projects[0].Agent.Options, "image_cache_capacity_mib") },
		"excess capacity":      func(c *config.Config) { c.Projects[0].Agent.Options["image_cache_capacity_mib"] = int64(512) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate, registered := partnerConfigFixture(t)
			mutate(candidate)
			if err := validateCRMProjectSet(candidate, registered); err == nil {
				t.Fatal("unsafe fixed configuration accepted")
			}
		})
	}
	// Old single-project configuration does not acquire four-environment requirements.
	single, _ := partnerConfigFixture(t)
	single.Projects = single.Projects[:1]
	delete(single.Projects[0].Agent.Options, "image_cache_capacity_mib")
	if err := validateCRMProjectSet(single, map[string]*crmfollowup.Adapter{single.Projects[0].Name: {}}); err != nil {
		t.Fatal(err)
	}
}
