// Package teambrain bridges authenticated action callbacks to the private Python host.
package teambrain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

const Kind = "knowledge"

func Commands() []string { return []string{"knowledge_search", "knowledge_read", "knowledge_propose"} }

type Adapter struct {
	command  string
	projects map[string]bool
}

// ValidateAgentUser keeps model execution under a different, non-root account.
func ValidateAgentUser(name string) error {
	account, err := user.Lookup(name)
	if err != nil {
		return errors.New("knowledge agent account not found")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid == 0 || uid == os.Geteuid() {
		return errors.New("knowledge agent account is not isolated")
	}
	return nil
}

func hostError(operation, category string) error {
	slog.Warn("knowledge host operation failed", "operation", operation, "category", category)
	return fmt.Errorf("knowledge host %s: %w", operation, errors.New(category))
}

func safeCode(value any) string {
	code, _ := value.(string)
	if len(code) == 0 || len(code) > 80 {
		return "invalid_host_status"
	}
	for _, c := range code {
		if (c < 'a' || c > 'z') && c != '_' {
			return "invalid_host_status"
		}
	}
	return code
}

// NewFromEnv consumes host-only configuration before agents are constructed.
// command is a protected absolute wrapper; config paths and secrets stay in it.
func NewFromEnv() (*Adapter, error) {
	command, projects := os.Getenv("CC_TEAM_BRAIN_COMMAND"), os.Getenv("CC_TEAM_BRAIN_PROJECTS")
	_ = os.Unsetenv("CC_TEAM_BRAIN_COMMAND")
	_ = os.Unsetenv("CC_TEAM_BRAIN_PROJECTS")
	if command == "" && projects == "" {
		return nil, nil
	}
	if err := validateCommand(command); err != nil {
		return nil, err
	}
	configured := map[string]bool{}
	for _, name := range strings.Split(projects, ",") {
		if name == "" || name != strings.TrimSpace(name) || configured[name] {
			return nil, errors.New("invalid knowledge project mapping")
		}
		configured[name] = true
	}
	if len(configured) != 6 {
		return nil, errors.New("knowledge host requires six explicit projects")
	}
	return &Adapter{command: command, projects: configured}, nil
}

func (a *Adapter) Enabled(project string) bool { return a != nil && a.projects[project] }

// RestartEnv restores host configuration for supervisor self-exec, never for
// model children. Duplicate inherited keys are replaced with the pinned values.
func (a *Adapter) RestartEnv(environment []string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "CC_TEAM_BRAIN_COMMAND=") && !strings.HasPrefix(entry, "CC_TEAM_BRAIN_PROJECTS=") {
			result = append(result, entry)
		}
	}
	projects := make([]string, 0, len(a.projects))
	for name := range a.projects {
		projects = append(projects, name)
	}
	sort.Strings(projects)
	return append(result, "CC_TEAM_BRAIN_COMMAND="+a.command, "CC_TEAM_BRAIN_PROJECTS="+strings.Join(projects, ","))
}

func (a *Adapter) Kind() string                            { return Kind }
func (a *Adapter) Match(core.Event) (core.ActionRef, bool) { return core.ActionRef{}, false }
func (a *Adapter) SessionEnv(token string) ([]string, error) {
	if token == "" {
		return nil, errors.New("missing knowledge session")
	}
	return []string{"TEAM_BRAIN_SESSION_TOKEN=" + token}, nil
}

func (a *Adapter) Begin(context.Context, core.ActionRef, core.ActionPrincipal, string, core.Language) (core.ActionHostResult, error) {
	return core.ActionHostResult{}, errors.New("knowledge proposals require the scoped tool")
}

func (a *Adapter) call(ctx context.Context, operation string, principal core.ActionPrincipal, extra map[string]any) (map[string]any, error) {
	if !a.Enabled(principal.Project) {
		return nil, errors.New("knowledge project disabled")
	}
	extra["operation"], extra["principal"] = operation, principal
	body, err := json.Marshal(extra)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, a.command)
	cmd.Dir = filepath.Dir(a.command)
	cmd.Stdin = bytes.NewReader(body)
	// The wrapper uses absolute paths. Do not forward inherited credentials.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONIOENCODING=utf-8"}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, hostError(operation, "stdout_pipe_failed")
	}
	if err := cmd.Start(); err != nil {
		category := "spawn_failed"
		if errors.Is(err, os.ErrNotExist) {
			category = "executable_missing"
		}
		if errors.Is(err, os.ErrPermission) {
			category = "executable_denied"
		}
		return nil, hostError(operation, category)
	}
	out, readErr := io.ReadAll(io.LimitReader(pipe, 2_000_001))
	if len(out) > 2_000_000 {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, hostError(operation, "request_cancelled")
	}
	if len(out) > 2_000_000 {
		return nil, hostError(operation, "output_too_large")
	}
	if readErr != nil {
		return nil, hostError(operation, "output_read_failed")
	}
	if waitErr != nil {
		return nil, hostError(operation, "process_failed")
	}
	var result map[string]any
	if json.Unmarshal(out, &result) != nil || result == nil {
		return nil, hostError(operation, "invalid_json_result")
	}
	return result, nil
}

func (a *Adapter) Tool(ctx context.Context, command string, input json.RawMessage, principal core.ActionPrincipal, token string, lang core.Language) (map[string]any, *core.ActionHostResult, error) {
	allowed := false
	for _, candidate := range Commands() {
		allowed = allowed || candidate == command
	}
	if !allowed || token == "" {
		return map[string]any{"status": "blocked", "code": "unsupported_tool_or_session"}, nil, nil
	}
	result, err := a.call(ctx, "tool", principal, map[string]any{"command": command, "input": input, "session_token": token})
	if err != nil || result["status"] != "pending" {
		return result, nil, err
	}
	if stringValue(result["preview"]) == "" {
		return nil, nil, hostError("tool", "missing_review_preview")
	}
	approval := present(result, lang)
	return result, &approval, nil
}

func (a *Adapter) BindCard(ctx context.Context, id string, principal core.ActionPrincipal) error {
	result, err := a.call(ctx, "bind", principal, map[string]any{"approval_id": id})
	if err != nil {
		return err
	}
	if result["status"] != "bound" {
		return hostError("bind", safeCode(result["code"]))
	}
	return nil
}

func (a *Adapter) Claim(ctx context.Context, id string, decision core.ActionDecision, principal core.ActionPrincipal, lang core.Language) (core.ActionHostResult, bool, error) {
	result, err := a.call(ctx, "claim", principal, map[string]any{"approval_id": id, "decision": decision})
	if err != nil {
		return core.ActionHostResult{}, false, err
	}
	execute, _ := result["execute"].(bool)
	return present(result, lang), execute, nil
}

func (a *Adapter) Execute(ctx context.Context, id string, principal core.ActionPrincipal, lang core.Language) (core.ActionHostResult, error) {
	result, err := a.call(ctx, "execute", principal, map[string]any{"approval_id": id})
	if err != nil {
		return core.ActionHostResult{}, err
	}
	return present(result, lang), nil
}
