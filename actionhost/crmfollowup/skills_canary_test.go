package crmfollowup

// Test-only filesystem installation and bounded native loading evidence. Skill
// bodies are never appended to the model prompt; normal CI never calls Claude.
import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func installCanarySkills(source, workspace, client string) (map[string]string, string, error) {
	hashes := map[string]string{}
	var contract strings.Builder
	for _, skill := range []string{"team-crm", "team-reminders"} {
		for _, name := range []string{"SKILL.md", "references/tools.md"} {
			relative := filepath.Join(skill, name)
			path := filepath.Join(source, relative)
			for current := path; ; current = filepath.Dir(current) {
				info, err := os.Lstat(current)
				if err != nil || info.Mode()&os.ModeSymlink != 0 {
					return nil, "", fmt.Errorf("missing or linked skill source: %s", relative)
				}
				if current == source {
					break
				}
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return nil, "", err
			}
			hashes[filepath.ToSlash(relative)] = fmt.Sprintf("%x", sha256.Sum256(body))
			contract.Write(body) // Preflight checks only, never passed to Claude.
			installed := strings.ReplaceAll(string(body), "/usr/local/bin/crm-tool", fmt.Sprintf("python3 %q", client))
			target := filepath.Join(workspace, ".claude", "skills", relative)
			if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, "", err
			}
			// The isolated runner uses umask 0077; make only instruction directories
			// traversable. They remain supervisor-owned, files model-read-only.
			for dir := filepath.Dir(target); dir != workspace; dir = filepath.Dir(dir) {
				if err = os.Chmod(dir, 0755); err != nil {
					return nil, "", err
				}
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
			if err != nil {
				return nil, "", err
			}
			_, writeErr := file.WriteString(installed)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				return nil, "", fmt.Errorf("cannot write isolated skill: %s", relative)
			}
			if err = os.Chmod(target, 0444); err != nil {
				return nil, "", err
			}
			hashes["installed/"+filepath.ToSlash(relative)] = fmt.Sprintf("%x", sha256.Sum256([]byte(installed)))
		}
	}
	return hashes, contract.String(), nil
}

type canarySkillEvidence struct {
	mu    sync.Mutex
	loads map[string]int
}

func (e *canarySkillEvidence) observe(event core.Event) {
	if event.Type != core.EventToolResult || event.ToolSuccess == nil || !*event.ToolSuccess {
		return
	}
	// Only record successful native tool results containing file-specific
	// headings. Do not retain arbitrary output, paths, thoughts or credentials.
	markers := map[string]string{
		"team-crm/SKILL.md":                  "# Team CRM",
		"team-crm/references/tools.md":       "## Controlled CRM tools: CRM_HOST_TOOLS_V1",
		"team-reminders/SKILL.md":            "# Team reminders",
		"team-reminders/references/tools.md": "## Controlled personal reminders: REMINDER_HOST_TOOLS_V1",
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loads == nil {
		e.loads = map[string]int{}
	}
	for file, marker := range markers {
		if strings.Contains(event.ToolResult, marker) {
			e.loads[file]++
		}
	}
}

func (e *canarySkillEvidence) snapshot() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := map[string]int{}
	for name, count := range e.loads {
		result[name] = count
	}
	return result
}

func (e *canarySkillEvidence) requireBeforeTool(t *testing.T, command string) {
	t.Helper()
	skill := "team-crm"
	if strings.HasPrefix(command, "reminder-") {
		skill = "team-reminders"
	} else if strings.HasPrefix(command, "knowledge-") {
		return // Existing knowledge fixture retains its separate loading evidence.
	}
	loaded := e.snapshot()
	for _, file := range []string{"SKILL.md", "references/tools.md"} {
		if loaded[skill+"/"+file] == 0 {
			t.Errorf("%s called without successful native loading of %s/%s", command, skill, file)
		}
	}
}

func TestCanarySkillsUseFilesystemAndSuccessfulResults(t *testing.T) {
	root := t.TempDir()
	source, workspace := filepath.Join(root, "source"), filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	for _, skill := range []string{"team-crm", "team-reminders"} {
		for _, name := range []string{"SKILL.md", "references/tools.md"} {
			path := filepath.Join(source, skill, name)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("fixture /usr/local/bin/crm-tool"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	hashes, contract, err := installCanarySkills(source, workspace, "/isolated/client.py")
	if err != nil || len(hashes) != 8 || !strings.Contains(contract, "/usr/local/bin/crm-tool") {
		t.Fatalf("bad installation: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, ".claude/skills/team-crm/references/tools.md"))
	if err != nil || !strings.Contains(string(body), `python3 "/isolated/client.py"`) {
		t.Fatalf("wrong isolated path substitution: %v", err)
	}
	if _, _, err := installCanarySkills(source, workspace, "/other"); err == nil {
		t.Fatal("must not overwrite a preexisting skill")
	}
	if _, _, err := installCanarySkills(filepath.Join(root, "missing"), workspace, "/other"); err == nil {
		t.Fatal("missing source accepted")
	}
	e := &canarySkillEvidence{}
	yes, no := true, false
	e.observe(core.Event{Type: core.EventToolUse, ToolInput: "# Team CRM"})
	e.observe(core.Event{Type: core.EventToolResult, ToolResult: "# Team CRM", ToolSuccess: &no})
	if len(e.snapshot()) != 0 {
		t.Fatal("an invocation/failed read is not successful skill loading")
	}
	e.observe(core.Event{Type: core.EventToolResult, ToolResult: "1→# Team CRM", ToolSuccess: &yes})
	if e.snapshot()["team-crm/SKILL.md"] != 1 {
		t.Fatal("successful native read not counted")
	}
}
