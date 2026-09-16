package crmfollowup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	Kind              = "crm.followup.v1"
	hostSecretEnv     = "MYANC_CRM_HOST_SECRET"
	hostSecretFileEnv = "MYANC_CRM_HOST_SECRET_FILE"
	actionTokenEnv    = "MYANC_CRM_ACTION_TOKEN"
	stageInputEnv     = "MYANC_CRM_STAGE_INPUT"
	projectEnv        = "MYANC_CRM_PROJECT"
	projectsEnv       = "MYANC_CRM_PROJECTS"
	commandEnv        = "MYANC_CRM_HOST_COMMAND"
	toolsEnv          = "MYANC_CRM_TOOLS"
	minHostSecretLen  = 32
	maxHostSecretLen  = 4096
	maxStageInputSize = 64 << 10
	markerCommand     = "/usr/local/bin/crm-followup"
	hostStageInputDir = "/var/lib/cc-action-inbox"
)

type commandRunner func(context.Context, string, string, []byte, []string) ([]byte, error)

// Adapter is the only business adapter in this fork. The helper owns its
// SQLite ledger, approval state machine, Feishu writes, and read-back receipt.
type Adapter struct {
	command         string
	hostSecret      string
	hostSecretFile  string
	workDir         string
	runAsUser       string
	runAsUID        int
	stageDir        string
	validateCommand bool
	toolsEnabled    bool
	run             commandRunner
	planBinding     func(core.ActionPrincipal) map[string]any
}

// NewFromEnv captures the host-only secret and removes it from the daemon
// environment before any Claude subprocess can inherit it.
func NewFromEnv() (*Adapter, string, error) {
	hosts, err := NewProjectHostsFromEnv()
	if err != nil {
		return nil, "", err
	}
	if len(hosts) > 1 {
		return nil, "", errors.New("multiple CRM projects require the shared listener")
	}
	for project, host := range hosts {
		return host, project, nil
	}
	return nil, "", nil
}

// NewProjectHostsFromEnv captures credentials once and returns an independent,
// initially unbound adapter per fixed project. It starts no process or listener.
func NewProjectHostsFromEnv() (map[string]*Adapter, error) {
	a, err := captureFromEnv()
	if err != nil {
		return nil, err
	}
	single, multiple := strings.TrimSpace(os.Getenv(projectEnv)), strings.TrimSpace(os.Getenv(projectsEnv))
	if single != "" && multiple != "" {
		return nil, errors.New("legacy and multiple CRM project settings are mutually exclusive")
	}
	if a == nil {
		if multiple != "" {
			return nil, errors.New("multiple CRM projects require a configured tools host")
		}
		return nil, nil
	}
	if multiple != "" && !a.toolsEnabled {
		return nil, errors.New("multiple CRM projects require tools mode")
	}
	names := []string{single}
	if multiple != "" {
		names = strings.Split(multiple, ",")
	}
	hosts := make(map[string]*Adapter, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || hosts[name] != nil || strings.ContainsAny(name, "/\\\x00\r\n") || name == "." || name == ".." {
			return nil, errors.New("CRM projects must have distinct non-empty names")
		}
		copy := *a
		hosts[name] = &copy
	}
	return hosts, nil
}

func captureFromEnv() (*Adapter, error) {
	secret := os.Getenv(hostSecretEnv)
	toolsMode := os.Getenv(toolsEnv)
	secretFile := strings.TrimSpace(os.Getenv(hostSecretFileEnv))
	for _, key := range []string{hostSecretEnv, hostSecretFileEnv, actionTokenEnv, stageInputEnv} {
		if err := os.Unsetenv(key); err != nil {
			return nil, fmt.Errorf("clear protected CRM environment: %w", err)
		}
	}
	if secret != "" && secretFile != "" {
		return nil, fmt.Errorf("set only one of %s or %s", hostSecretEnv, hostSecretFileEnv)
	}
	if secretFile != "" {
		if strings.ContainsRune(secretFile, '\x00') || !filepath.IsAbs(secretFile) {
			return nil, fmt.Errorf("%s must be an absolute path", hostSecretFileEnv)
		}
		value, err := readHostSecretFile(secretFile)
		if err != nil {
			return nil, fmt.Errorf("read CRM host secret file: %w", err)
		}
		secret = strings.TrimSpace(string(value))
		if secret == "" {
			return nil, errors.New("CRM host secret file is empty")
		}
	}
	if secret == "" {
		if toolsMode != "" {
			return nil, errors.New("CRM tools require a configured action host")
		}
		return nil, nil
	}
	if toolsMode != "" && toolsMode != "1" {
		return nil, fmt.Errorf("%s must be absent or exactly 1", toolsEnv)
	}
	if len(secret) < minHostSecretLen || len(secret) > maxHostSecretLen {
		return nil, fmt.Errorf("CRM host secret must be %d..%d bytes", minHostSecretLen, maxHostSecretLen)
	}
	command := strings.TrimSpace(os.Getenv(commandEnv))
	if command == "" {
		return nil, fmt.Errorf("%s is required when CRM action host is enabled", commandEnv)
	}
	if command != markerCommand {
		return nil, fmt.Errorf("%s must be exactly %s", commandEnv, markerCommand)
	}
	return &Adapter{
		command: command, hostSecret: secret, hostSecretFile: secretFile, stageDir: hostStageInputDir,
		toolsEnabled:    toolsMode == "1",
		validateCommand: true, run: runCommand,
	}, nil
}

func (a *Adapter) Kind() string { return Kind }

func (a *Adapter) ToolsEnabled() bool { return a.toolsEnabled }

// RestartEnv is only for re-exec of cc-connect itself after its engines stop.
// Never restore secrets in os.Environ: unrelated children must not inherit them.
// The new supervisor immediately captures/removes these again in NewFromEnv.
func (a *Adapter) RestartEnv(base []string) []string {
	env := withoutEnv(base, hostSecretEnv, hostSecretFileEnv, actionTokenEnv, stageInputEnv)
	if a.hostSecretFile != "" {
		return append(env, hostSecretFileEnv+"="+a.hostSecretFile)
	}
	return append(env, hostSecretEnv+"="+a.hostSecret)
}

// EnableTools selects the controlled tool route before initialization. Legacy
// deployments keep their marker/inbox route unless they explicitly opt in.
func (a *Adapter) EnableTools() error {
	if a.workDir != "" {
		return errors.New("CRM tool mode must be selected before workspace initialization")
	}
	a.toolsEnabled = true
	return nil
}

// SetWorkDir binds the adapter to the configured project workspace. It must
// be called once before the adapter is attached to an engine.
func (a *Adapter) SetWorkDir(workDir, runAsUser string) error {
	if a.workDir != "" {
		return errors.New("CRM adapter workspace is already bound")
	}
	workDir = filepath.Clean(strings.TrimSpace(workDir))
	if workDir == "." || !filepath.IsAbs(workDir) {
		return errors.New("CRM action host work directory must be absolute")
	}
	runAsUser = strings.TrimSpace(runAsUser)
	if runAsUser == "" {
		return errors.New("CRM action host requires a non-empty run_as_user")
	}
	uid, err := resolveRunAsOwner(runAsUser)
	if err != nil {
		return fmt.Errorf("resolve CRM run_as_user: %w", err)
	}
	a.workDir = workDir
	a.runAsUser = runAsUser
	a.runAsUID = uid
	if a.validateCommand {
		if err := validateHostCommandPath(a.command); err != nil {
			return fmt.Errorf("validate CRM host helper: %w", err)
		}
	}
	if a.toolsEnabled {
		return nil
	}
	return a.inspectStageInputDir()
}

// Match recognizes only the side-effect-free handoff argv owned by this
// adapter. Core never needs to know a business command or action kind.
func (a *Adapter) Match(event core.Event) (core.ActionRef, bool) {
	if a.toolsEnabled || event.Type != core.EventPermissionRequest || event.ToolName != "Bash" {
		return core.ActionRef{}, false
	}
	command, ok := event.ToolInputRaw["command"].(string)
	if !ok || command != markerCommand+" request-approval" {
		return core.ActionRef{}, false
	}
	return core.ActionRef{Kind: Kind}, true
}

// SessionEnv exposes only the per-session staging token to the agent process.
func (a *Adapter) SessionEnv(token string) ([]string, error) {
	if token == "" {
		return nil, errors.New("CRM action token is required")
	}
	if a.workDir == "" || a.runAsUser == "" {
		return nil, errors.New("CRM action host work directory is not configured")
	}
	if a.toolsEnabled {
		return []string{actionTokenEnv + "=" + token}, nil
	}
	if err := a.inspectStageInputDir(); err != nil {
		return nil, err
	}
	inputPath := a.stageInputPath(token)
	return []string{
		actionTokenEnv + "=" + token,
		stageInputEnv + "=" + inputPath,
	}, nil
}

func (a *Adapter) Begin(ctx context.Context, ref core.ActionRef, principal core.ActionPrincipal, token string, lang core.Language) (core.ActionHostResult, error) {
	if a.toolsEnabled || ref.Kind != Kind || ref.ChangeID != "" || token == "" {
		return core.ActionHostResult{}, errors.New("invalid hosted action begin")
	}
	request, err := a.consumeStageInput(token)
	if err != nil {
		return core.ActionHostResult{}, err
	}
	payload := struct {
		Request   json.RawMessage      `json:"request"`
		Principal core.ActionPrincipal `json:"principal"`
	}{request, principal}
	result, err := a.call(ctx, "host-prepare", payload, token)
	if err != nil {
		return core.ActionHostResult{}, err
	}
	response := hostResult(result)
	response.Kind = Kind
	response.Card = approvalCard(result, lang)
	return response, nil
}

func (a *Adapter) stageInputPath(token string) string {
	digest := sha256.Sum256([]byte(token))
	return filepath.Join(a.inputDir(), fmt.Sprintf("%x.json", digest))
}

func (a *Adapter) inputDir() string {
	if a.stageDir != "" {
		return a.stageDir
	}
	// Tests construct adapters directly. Production adapters created by
	// NewFromEnv always use the fixed host-owned inbox above.
	return filepath.Join(a.workDir, ".crm-input")
}

func (a *Adapter) inspectStageInputDir() error {
	inputDir := a.inputDir()
	info, err := os.Lstat(inputDir)
	if err != nil {
		return fmt.Errorf("inspect CRM stage input directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("CRM stage input path is not a directory")
	}
	return validateStageInputDir(info)
}

func (a *Adapter) consumeStageInput(token string) (json.RawMessage, error) {
	if err := a.inspectStageInputDir(); err != nil {
		return nil, err
	}
	inputDir := a.inputDir()
	inputPath := a.stageInputPath(token)
	name := filepath.Base(inputPath)
	root, err := os.OpenRoot(inputDir)
	if err != nil {
		return nil, fmt.Errorf("open CRM stage input directory: %w", err)
	}
	defer func() { _ = root.Close() }() // Read-only directory handle; no buffered writes.
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("inspect open CRM stage input directory: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("open CRM stage input path is not a directory")
	}
	if err := validateStageInputDir(rootInfo); err != nil {
		return nil, err
	}
	linkInfo, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect CRM stage input: %w", err)
	}
	if !linkInfo.Mode().IsRegular() || linkInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("CRM stage input is not a regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open CRM stage input: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open CRM stage input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxStageInputSize {
		return nil, errors.New("CRM stage input size or type is invalid")
	}
	if err := validateStageInputFile(info, a.runAsUID); err != nil {
		return nil, err
	}
	request, err := io.ReadAll(io.LimitReader(file, maxStageInputSize+1))
	if err != nil {
		return nil, fmt.Errorf("read CRM stage input: %w", err)
	}
	if len(request) > maxStageInputSize {
		return nil, errors.New("CRM stage input is too large")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(request, &object); err != nil || object == nil {
		return nil, errors.New("CRM stage input must be a JSON object")
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close CRM stage input: %w", err)
	}
	closed = true
	if err := root.Remove(name); err != nil {
		return nil, fmt.Errorf("consume CRM stage input: %w", err)
	}
	return json.RawMessage(request), nil
}

func (a *Adapter) Claim(ctx context.Context, approvalID string, decision core.ActionDecision, principal core.ActionPrincipal, lang core.Language) (core.ActionHostResult, bool, error) {
	if approvalID == "" {
		return core.ActionHostResult{}, false, errors.New("approval id is required")
	}
	if decision != core.ActionApprove && decision != core.ActionModify && decision != core.ActionCancel {
		return core.ActionHostResult{}, false, errors.New("invalid hosted action decision")
	}
	payload := struct {
		ApprovalID string               `json:"approval_id"`
		Decision   core.ActionDecision  `json:"decision"`
		Principal  core.ActionPrincipal `json:"principal"`
	}{approvalID, decision, principal}
	result, err := a.call(ctx, "host-claim", payload, "")
	if err != nil {
		return core.ActionHostResult{}, false, err
	}
	response := hostResult(result)
	response.Kind = Kind
	response.ApprovalID = approvalID
	execute, _ := result["execute"].(bool)
	if response.Status == "executing" {
		// Only the callback that won the durable CAS may advance the card to
		// "executing". A duplicate or conflicting click while that worker is
		// running must be toast-only, otherwise its delayed response can replace
		// an already verified receipt with stale UI.
		if execute {
			response.Card = executingCard(result, lang)
		}
	} else {
		response.Card = receiptCard(result, lang)
	}
	if execute && response.Status != "executing" {
		return core.ActionHostResult{}, false, errors.New("CRM helper requested execution without a claimed approval")
	}
	return response, execute, nil
}

func (a *Adapter) BindCard(ctx context.Context, approvalID string, principal core.ActionPrincipal) error {
	if approvalID == "" || principal.MessageID == "" {
		return errors.New("approval and card message ids are required")
	}
	payload := struct {
		ApprovalID string               `json:"approval_id"`
		Principal  core.ActionPrincipal `json:"principal"`
	}{approvalID, principal}
	result, err := a.call(ctx, "host-bind-card", payload, "")
	if err != nil {
		return err
	}
	if status, _ := result["status"].(string); status != "bound" {
		return errors.New("CRM helper did not bind the emitted approval card")
	}
	return nil
}

func (a *Adapter) Execute(ctx context.Context, approvalID string, principal core.ActionPrincipal, lang core.Language) (core.ActionHostResult, error) {
	if approvalID == "" {
		return core.ActionHostResult{}, errors.New("approval id is required")
	}
	payload := struct {
		ApprovalID string               `json:"approval_id"`
		Principal  core.ActionPrincipal `json:"principal"`
	}{approvalID, principal}
	result, err := a.call(ctx, "host-execute", payload, "")
	if err != nil {
		return core.ActionHostResult{}, err
	}
	response := hostResult(result)
	response.Kind = Kind
	response.ApprovalID = approvalID
	response.Card = receiptCard(result, lang)
	if next, ok := result["next_approval"].(map[string]any); ok && response.Status == "verified" {
		// Only Execute's first durable worker may supply this transient child.
		// Claim/replay never reconstructs or republishes it from a receipt.
		if text(next["status"]) == "pending" && next["next_approval"] == nil {
			child := hostResult(next)
			child.Kind = Kind
			child.Card = approvalCard(next, lang)
			response.Next = &child
		}
	}
	return response, nil
}

func (a *Adapter) call(ctx context.Context, subcommand string, input any, actionToken string) (map[string]any, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", subcommand, err)
	}
	env := withoutEnv(os.Environ(), hostSecretEnv, hostSecretFileEnv, actionTokenEnv, stageInputEnv)
	env = append(env, hostSecretEnv+"="+a.hostSecret)
	if actionToken != "" {
		env = append(env, actionTokenEnv+"="+actionToken)
	}
	out, err := a.run(ctx, a.command, subcommand, payload, env)
	if err != nil {
		return nil, fmt.Errorf("CRM helper %s failed: %w", subcommand, err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("CRM helper %s returned invalid JSON: %w", subcommand, err)
	}
	return result, nil
}

func runCommand(ctx context.Context, command, subcommand string, input []byte, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, subcommand)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}

func withoutEnv(env []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, found := blocked[key]; found {
				continue
			}
		}
		clean = append(clean, entry)
	}
	return clean
}

func hostResult(result map[string]any) core.ActionHostResult {
	replayed, _ := result["replayed"].(bool)
	return core.ActionHostResult{
		Replayed:   replayed,
		Status:     text(result["status"]),
		Code:       text(result["code"]),
		ApprovalID: text(result["approval_id"]),
		ChangeID:   text(result["change_id"]),
	}
}

func approvalCard(result map[string]any, lang core.Language) *core.Card {
	i18n := core.NewI18n(lang)
	status := text(result["status"])
	if status != "pending" {
		return stageOutcomeCard(result, i18n)
	}
	approvalID := text(result["approval_id"])
	preview, _ := result["preview"].(map[string]any)
	title := core.MsgCRMApprovalTitle
	switch text(preview["operation_type"]) {
	case "customer_create":
		title = core.MsgCRMCreateApprovalTitle
	case "customer_update":
		title = core.MsgCRMUpdateApprovalTitle
	}
	b := core.NewCard().Title(i18n.T(title), "blue")
	b.Markdown(renderPreview(preview, i18n))
	b.Divider().ButtonsEqual(
		actionButton(i18n.T(core.MsgCRMApproveButton), "primary", approvalID, core.ActionApprove, lang),
		actionButton(i18n.T(core.MsgCRMModifyButton), "default", approvalID, core.ActionModify, lang),
		actionButton(i18n.T(core.MsgCRMCancelButton), "danger", approvalID, core.ActionCancel, lang),
	)
	return b.Build()
}

func stageOutcomeCard(result map[string]any, i18n *core.I18n) *core.Card {
	switch text(result["status"]) {
	case "needs_input":
		return core.NewCard().Title(i18n.T(core.MsgCRMNeedsInputTitle), "blue").Markdown(i18n.T(core.MsgCRMNeedsInputBody)).Build()
	case "needs_time":
		return core.NewCard().
			Title(i18n.T(core.MsgCRMNeedsTimeTitle), "blue").
			Markdown(i18n.T(core.MsgCRMNeedsTimeBody)).
			Build()
	case "needs_selection":
		return core.NewCard().
			Title(i18n.T(core.MsgCRMNeedsSelectionTitle), "blue").
			Markdown(i18n.T(core.MsgCRMNeedsSelectionBody) + "\n\n" + renderCandidates(result["candidates"], i18n)).
			Build()
	case "not_found":
		return core.NewCard().
			Title(i18n.T(core.MsgCRMNotFoundTitle), "grey").
			Markdown(i18n.Tf(core.MsgCRMNotFoundBodyFmt, display(result["customer_query"]))).
			Build()
	case "blocked":
		return core.NewCard().
			Title(i18n.T(core.MsgCRMBlockedTitle), "red").
			Markdown(i18n.T(core.MsgCRMBlockedBody)).
			Build()
	default:
		return core.NewCard().
			Title(i18n.T(core.MsgCRMApprovalFailedTitle), "red").
			Markdown(i18n.T(core.MsgCRMApprovalFailedBody)).
			Build()
	}
}

func renderCandidates(value any, i18n *core.I18n) string {
	candidates, _ := value.([]any)
	if len(candidates) == 0 {
		return i18n.T(core.MsgCRMNoFields)
	}
	blocks := make([]string, 0, len(candidates))
	for index, raw := range candidates {
		candidate, _ := raw.(map[string]any)
		blocks = append(blocks,
			i18n.Tf(core.MsgCRMCandidateHeadingFmt, index+1)+"\n"+
				renderFields(candidate, []string{"customer_number", "name", "contact", "stage", "owner"}, i18n),
		)
	}
	return strings.Join(blocks, "\n\n")
}

func actionButton(label, typ, approvalID string, decision core.ActionDecision, lang core.Language) core.CardButton {
	return core.CardButton{
		Text: label,
		Type: typ,
		Extra: map[string]string{
			"kind":        Kind,
			"approval_id": approvalID,
			"decision":    string(decision),
			"language":    string(lang),
		},
	}
}

func receiptCard(result map[string]any, lang core.Language) *core.Card {
	i18n := core.NewI18n(lang)
	status := text(result["status"])
	title, color := receiptTitle(status, i18n)
	parent := relatedParent(result)
	repairClosed := text(result["code"]) == "repair_closed_partial" && (status == "cancelled" || status == "superseded" || status == "expired")
	if repairClosed {
		title, color = i18n.T(core.MsgCRMRepairClosedTitle), "orange"
	} else if parent != nil && (status == "cancelled" || status == "superseded" || status == "expired" || status == "replan_required") {
		title = i18n.T(core.MsgCRMChildCancelledTitle)
	}
	b := core.NewCard().Title(title, color)
	if repairClosed {
		b.Markdown(i18n.T(core.MsgCRMRepairClosedBody))
	} else {
		b.Markdown(receiptMessage(status, text(result["code"]), i18n))
	}
	if parent != nil {
		b.Markdown(i18n.T(core.MsgCRMParentUnaffectedBody))
	}
	if next := text(result["next_approval_status"]); next == "unavailable" || next == "not_published" {
		b.Markdown(i18n.T(core.MsgCRMFollowupUnpublishedBody))
	} else if text(result["child_change_id"]) != "" || next != "" {
		b.Markdown(i18n.T(core.MsgCRMFollowupSeparateBody))
	}
	if profile, ok := result["customer_profile"].(map[string]any); ok {
		b.Divider().Markdown(renderCustomerProfile(profile, true, i18n))
	}
	if followup, ok := result["followup"].(map[string]any); ok {
		b.Divider().Markdown(i18n.T(core.MsgCRMReceiptFollowupHeading) + "\n" + renderFields(followup, []string{"occurred_at", "channel", "content", "next_action", "next_followup_at"}, i18n))
	}
	if customer, ok := result["customer"].(map[string]any); ok {
		b.Divider().Markdown(i18n.T(core.MsgCRMReceiptCustomerHeading) + "\n" + renderFields(customer, []string{"customer_number", "name", "contact", "phone", "email", "stage", "owner", "last_followup_at", "last_followup_content", "next_action", "next_followup_at"}, i18n))
		if link := safeLink(customer["url"]); link != "" {
			b.Markdown("[" + i18n.T(core.MsgCRMOpenCustomer) + "](" + link + ")")
		}
	}
	if changes, ok := result["customer_changes"].([]any); ok && len(changes) > 0 {
		var lines []string
		for _, raw := range changes {
			change, _ := raw.(map[string]any)
			mark := "⚠️"
			if verified, _ := change["verified"].(bool); verified {
				mark = "✅"
			}
			field := text(change["field"])
			if fieldLabel(field, i18n) == "" {
				continue
			}
			actual := i18n.T(core.MsgCRMNotReadBack)
			if value, present := change["actual"]; present {
				actual = displayField(field, value, i18n)
			}
			lines = append(lines, fmt.Sprintf("%s **%s**: %s → %s", mark, fieldLabel(field, i18n), displayField(field, change["before"], i18n), actual))
		}
		b.Divider().Markdown(i18n.T(core.MsgCRMFieldVerificationHeading) + "\n" + strings.Join(lines, "\n"))
	}
	if id := text(result["change_id"]); id != "" {
		b.Divider().Note(i18n.Tf(core.MsgCRMOperationNoteFmt, id, status))
	}
	if text(result["plan_reminder_status"]) != "" {
		b.Markdown(i18n.T(core.MsgCRMPlanReceipt))
	}
	return b.Build()
}

func executingCard(result map[string]any, lang core.Language) *core.Card {
	i18n := core.NewI18n(lang)
	b := core.NewCard().
		Title(i18n.T(core.MsgHostedActionExecutingTitle), "blue").
		Markdown(i18n.T(core.MsgHostedActionExecutingBody)).
		Markdown(i18n.T(core.MsgCRMWriteCoordination))
	// Only the ledger's frozen preview is authoritative, never callback values.
	if preview, ok := result["preview"].(map[string]any); ok {
		b.Divider().Markdown(renderPreview(preview, i18n))
	}
	return b.Divider().Markdown(i18n.T(core.MsgHostedActionExecutingBody)).Build()
}

func receiptTitle(status string, i18n *core.I18n) (string, string) {
	switch status {
	case "verified":
		return i18n.T(core.MsgCRMReceiptVerifiedTitle), "green"
	case "partial":
		return i18n.T(core.MsgCRMReceiptPartialTitle), "orange"
	case "verification_unknown", "executing":
		return i18n.T(core.MsgCRMReceiptUnknownTitle), "orange"
	case "cancelled":
		return i18n.T(core.MsgCRMReceiptCancelledTitle), "grey"
	case "superseded":
		return i18n.T(core.MsgCRMReceiptSupersededTitle), "blue"
	case "expired", "replan_required":
		return i18n.T(core.MsgCRMReceiptExpiredTitle), "orange"
	default:
		return i18n.T(core.MsgHostedActionFailedTitle), "red"
	}
}

// The helper's stable status/code select business copy; its diagnostic message
// is never presentation authority or safe to relay to a chat.
func receiptMessage(status, code string, i18n *core.I18n) string {
	if code == "crm_write_unresolved" {
		return i18n.T(core.MsgCRMWriteUnresolved)
	}
	if code == "crm_write_busy" {
		return i18n.T(core.MsgCRMWriteBusy)
	}
	key := core.MsgCRMReceiptReviewBody
	switch status {
	case "verified":
		switch code {
		case "followup_and_customer_verified":
			key = core.MsgCRMReceiptVerifiedBody
		case "customer_created_verified":
			key = core.MsgCRMCreatedBody
		case "customer_updated_verified":
			key = core.MsgCRMUpdatedBody
		case "followup_recorded_customer_unchanged":
			key = core.MsgCRMReceiptNewerBody
		case "already_applied", "partial_already_completed":
			key = core.MsgCRMReceiptReplayBody
		}
	case "partial":
		key = core.MsgCRMReceiptPartialBody
		switch code {
		case "customer_changed_after_followup", "customer_changed_after_partial", "schema_changed_after_partial":
			key = core.MsgCRMReceiptPartialDriftBody
		}
	case "verification_unknown":
		switch code {
		case "followup_write_unconfirmed", "followup_readback_mismatch":
			key = core.MsgCRMReceiptFollowupUnknownBody
		case "customer_write_unconfirmed":
			key = core.MsgCRMReceiptCustomerUnknownBody
		}
	case "executing":
		if code != "execution_result_unknown" {
			key = core.MsgHostedActionExecutingBody
		}
	case "cancelled":
		key = core.MsgCRMReceiptCancelledBody
	case "superseded":
		key = core.MsgCRMReceiptSupersededBody
	case "expired":
		key = core.MsgCRMReceiptExpiredBody
	case "replan_required":
		key = core.MsgCRMReceiptReplanBody
		if code == "expired" {
			key = core.MsgCRMReceiptExpiredBody
		}
	case "blocked":
		key = core.MsgCRMReceiptBlockedBody
	case "failed":
		if code == "prewrite_read_failed" {
			key = core.MsgCRMReceiptPrewriteBody
		}
	}
	return i18n.T(key)
}

func renderPreview(preview map[string]any, i18n *core.I18n) string {
	if op := text(preview["operation_type"]); op == "customer_create" || op == "customer_update" {
		return renderCustomerPreview(preview, i18n)
	}
	var sections []string
	if text(preview["parent_change_id"]) != "" {
		sections = append(sections, i18n.T(core.MsgCRMParentUnaffectedBody))
	}
	repairing := repairPreview(preview)
	if actor := text(preview["actor"]); actor != "" {
		sections = append(sections, i18n.Tf(core.MsgCRMPreviewActorFmt, display(actor)))
	}
	if customer, ok := preview["customer"].(map[string]any); ok {
		sections = append(sections, i18n.T(core.MsgCRMPreviewTargetHeading)+"\n"+renderFields(customer, []string{"customer_number", "name", "contact", "phone", "email", "stage", "owner", "last_followup_at", "last_followup_content", "next_action", "next_followup_at"}, i18n))
		if link := safeLink(customer["url"]); link != "" {
			sections = append(sections, "["+i18n.T(core.MsgCRMOpenCustomer)+"]("+link+")")
		}
	}
	if effects, ok := preview["effects"].(map[string]any); ok {
		if followup, ok := effects["followup"].(map[string]any); ok {
			if after, ok := followup["after"].(map[string]any); ok {
				heading := core.MsgCRMPreviewNewFollowupHeading
				if repairing {
					heading = core.MsgCRMPreviewExistingFollowupHeading
				}
				sections = append(sections, i18n.T(heading)+"\n"+renderFields(after, []string{"occurred_at", "channel", "content", "next_action", "next_followup_at"}, i18n))
			}
		}
		if customerEffect, ok := effects["customer"].(map[string]any); ok {
			if changes, ok := customerEffect["changes"].([]any); ok && len(changes) > 0 {
				var lines []string
				for _, raw := range changes {
					change, _ := raw.(map[string]any)
					field := text(change["field"])
					if fieldLabel(field, i18n) == "" {
						continue
					}
					lines = append(lines, fmt.Sprintf("- **%s**: %s → %s", fieldLabel(field, i18n), displayField(field, change["before"], i18n), displayField(field, change["after"], i18n)))
				}
				sections = append(sections, i18n.T(core.MsgCRMPreviewChangesHeading)+"\n"+strings.Join(lines, "\n"))
			}
		}
	}
	if expiry := text(preview["expires_at"]); expiry != "" {
		expiryKey := core.MsgCRMPreviewExpiryBlockFmt
		if repairing {
			expiryKey = core.MsgCRMPreviewRepairExpiryBlockFmt
		}
		sections = append(sections, i18n.Tf(expiryKey, displayField("expires_at", expiry, i18n)))
	}
	if plan := renderPlanReminder(preview, i18n); plan != "" {
		sections = append(sections, plan)
	}
	return strings.Join(sections, "\n\n")
}

func repairPreview(preview map[string]any) bool {
	if effects, ok := preview["effects"].(map[string]any); ok {
		if followup, ok := effects["followup"].(map[string]any); ok && text(followup["action"]) == "already_verified" {
			return true
		}
	}
	order, present := preview["execution_order"]
	if !present {
		return false
	}
	switch values := order.(type) {
	case []any:
		for _, value := range values {
			if text(value) == "create_followup" {
				return false
			}
		}
	case []string:
		for _, value := range values {
			if value == "create_followup" {
				return false
			}
		}
	default:
		return false
	}
	return true
}

func renderFields(values map[string]any, order []string, i18n *core.I18n) string {
	var lines []string
	for _, key := range order {
		value, ok := values[key]
		if !ok || (key != "owner" && (value == nil || text(value) == "")) {
			continue
		}
		lines = append(lines, "- **"+fieldLabel(key, i18n)+"**: "+displayField(key, value, i18n))
	}
	if len(lines) == 0 {
		return i18n.T(core.MsgCRMNoFields)
	}
	return strings.Join(lines, "\n")
}

func displayField(field string, value any, i18n *core.I18n) string {
	if field == "owner" {
		return displayOwner(value, i18n)
	}
	if value == nil || text(value) == "" {
		return i18n.T(core.MsgCRMUnset)
	}
	switch field {
	case "occurred_at", "last_followup_at", "next_followup_at", "created_at", "updated_at", "expires_at":
		if parsed, err := time.Parse(time.RFC3339Nano, stripFence(text(value))); err == nil {
			layout := "2006-01-02 15:04"
			if field == "expires_at" {
				layout += ":05"
			}
			return i18n.Tf(core.MsgCRMBeijingTimeFmt, parsed.In(time.FixedZone("UTC+08:00", 8*60*60)).Format(layout))
		}
	}
	return display(value)
}

func displayOwner(value any, i18n *core.I18n) string {
	switch owner := value.(type) {
	case nil:
		return i18n.T(core.MsgCRMUnset)
	case []any:
		if len(owner) == 0 {
			return i18n.T(core.MsgCRMUnset)
		}
		names := make([]string, 0, len(owner))
		for _, person := range owner {
			names = append(names, displayOwner(person, i18n))
		}
		return strings.Join(names, i18n.T(core.MsgCRMNameSeparator))
	case map[string]any:
		if name, ok := owner["name"].(string); ok && strings.TrimSpace(stripFence(name)) != "" {
			return display(name)
		}
	case string:
		name := strings.TrimSpace(stripFence(owner))
		if name == "" {
			return i18n.T(core.MsgCRMUnset)
		}
		if !strings.HasPrefix(name, "ou_") && !strings.HasPrefix(name, "on_") {
			return display(name)
		}
	}
	return i18n.T(core.MsgCRMOwnerUnavailable)
}

func fieldLabel(field string, i18n *core.I18n) string {
	labels := map[string]core.MsgKey{
		"customer_number": core.MsgCRMFieldCustomerNumber, "name": core.MsgCRMFieldName, "contact": core.MsgCRMFieldContact,
		"phone": core.MsgCRMFieldPhone, "email": core.MsgCRMFieldEmail, "stage": core.MsgCRMFieldStage, "owner": core.MsgCRMFieldOwner,
		"last_followup_at": core.MsgCRMFieldLastFollowupAt, "last_followup_content": core.MsgCRMFieldLastFollowupContent,
		"next_action": core.MsgCRMFieldNextAction, "next_followup_at": core.MsgCRMFieldNextFollowupAt,
		"occurred_at": core.MsgCRMFieldOccurredAt, "channel": core.MsgCRMFieldChannel, "content": core.MsgCRMFieldContent,
	}
	if key := labels[field]; key != "" {
		return i18n.T(key)
	}
	return ""
}

func display(value any) string { return escapeMarkdown(stripFence(text(value))) }

func text(value any) string {
	if value == nil {
		return ""
	}
	if value, ok := value.(string); ok {
		return value
	}
	return fmt.Sprint(value)
}

func stripFence(value string) string {
	value = strings.TrimPrefix(value, "<CRM_DATA>")
	return strings.TrimSuffix(value, "</CRM_DATA>")
}

func escapeMarkdown(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}

func safeLink(value any) string {
	raw := stripFence(text(value))
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	// Parentheses are legal in URLs but terminate Markdown link targets.
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(parsed.String())
}
