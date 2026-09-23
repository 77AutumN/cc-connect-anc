package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

const MaxFileBytes = 20 << 20

type Sender func(context.Context, json.RawMessage, core.FileAttachment, string) (string, error)

type Binding = core.FileWorkBinding
type Input = core.FileWorkInput
type Artifact = core.FileWorkArtifact
type WorkContext = core.FileWorkContext
type WorkRef = core.FileWorkRef

var _ core.FileWorkHost = (*Host)(nil)

type delivery struct {
	Artifact
	RequestKey string
	Snapshot   string
}

type work struct {
	ID, SessionID, Root, RootIdentity, InputIdentity, OutputIdentity string
	Principal                                                        core.ActionPrincipal
	Route                                                            json.RawMessage
	OwnerUID                                                         int
	Inputs                                                           []Input
	PendingInputs                                                    map[string][]Input `json:",omitempty"`
	Version                                                          int
	Deliveries                                                       map[string]*delivery
	Messages                                                         map[string]bool
	GroupRealm                                                       string `json:",omitempty"`
}

type Host struct {
	store             *Store
	snapshots         *os.Root
	send              Sender
	groupRealm        string
	documentValidator string
}

// SetGroupReferences is a startup-only opt-in for one fixed Bot/group realm.
// The host never derives sharing authority from model-supplied arguments.
func (h *Host) SetGroupReferences(realm string) error {
	if !validHash(realm) {
		return ErrInvalid
	}
	h.groupRealm = realm
	return nil
}

func New(store *Store, snapshotDir string, send Sender) (*Host, error) {
	if store == nil || send == nil {
		return nil, ErrUnavailable
	}
	r, err := protectedRoot(snapshotDir)
	if err != nil {
		return nil, err
	}
	info, err := r.Stat(".")
	if err != nil || info.Mode().Perm()&0077 != 0 {
		_ = r.Close()
		return nil, ErrUnavailable
	}
	return &Host{store: store, snapshots: r, send: send}, nil
}

func (h *Host) Close() error { return h.snapshots.Close() }

func validPrincipal(p core.ActionPrincipal) bool {
	return p.Platform != "" && p.UserID != "" && p.ChatID != "" && p.SessionKey != "" && p.Project != "" && p.MessageID != ""
}

func scope(p core.ActionPrincipal) string {
	b, _ := json.Marshal([]string{p.Platform, p.UserID, p.ChatID, p.SessionKey, p.Project})
	return hash(b)
}

func hash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}

func workContext(w *work) WorkContext {
	r := WorkContext{Enabled: true, WorkID: w.ID, WorkRoot: w.Root, Inputs: append([]Input{}, w.Inputs...), OutputDir: filepath.Join(w.Root, "outputs"), LatestVersion: w.Version, Artifacts: []Artifact{}}
	for version := 1; version <= w.Version; version++ {
		for _, d := range w.Deliveries {
			if d.Version == version {
				r.Artifacts = append(r.Artifacts, d.Artifact)
			}
		}
	}
	return r
}

// Bind accepts only transport/launcher values. One native session owns one work;
// later turns preserve the first message/route and may add selected attachments.
func (h *Host) Bind(ctx context.Context, b Binding) (WorkContext, error) {
	var result WorkContext
	if !validPrincipal(b.Principal) || b.SessionID == "" || b.OwnerUID < 0 || !json.Valid(b.Route) || bytes.Equal(bytes.TrimSpace(b.Route), []byte("null")) {
		return result, ErrInvalid
	}
	if len(b.Inputs) > 4 {
		return result, ErrInvalid
	}
	total := 0
	for _, f := range b.Inputs {
		total += len(f.Data)
		if f.ReceiveError != "" || len(f.Data) > MaxFileBytes || total > 2*MaxFileBytes || !safeName(f.FileName) {
			return result, ErrInvalid
		}
		if err := h.validateFile(ctx, f.FileName, f.Data); err != nil {
			if errors.Is(err, ErrUnavailable) {
				return result, core.NewFileInputError(core.MsgFileInputUnavailable)
			}
			return result, core.NewFileInputError(core.MsgFileInputFormatUnsupported)
		}
	}
	r, err := protectedRoot(b.WorkRoot)
	if err != nil {
		return result, err
	}
	defer func() { _ = r.Close() }()
	rootInfo, err := r.Stat(".")
	if err != nil {
		return result, ErrUnavailable
	}
	inputs, inputInfo, err := childRoot(r, "inputs", os.Geteuid(), false)
	if err != nil {
		return result, err
	}
	defer func() { _ = inputs.Close() }()
	outputs, outputInfo, err := childRoot(r, "outputs", b.OwnerUID, true)
	if err != nil {
		return result, err
	}
	_ = outputs.Close()
	err = h.store.change(ctx, func(st *state) error {
		var source *core.FileWorkSource
		if b.SourceReceipt != "" {
			if !b.Group {
				return ErrScope
			}
			w, d, err := h.groupArtifact(st, b.Principal, b.SourceReceipt)
			if err != nil {
				return err
			}
			f, err := h.snapshots.Open(d.Snapshot)
			if err != nil {
				return ErrUnavailable
			}
			data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
			_ = f.Close()
			if err != nil || len(data) > MaxFileBytes || total+len(data) > 2*MaxFileBytes || len(b.Inputs) >= 4 || hash(data) != d.SHA256 {
				return ErrInvalid
			}
			if err := h.validateFile(ctx, d.Name, data); err != nil {
				return err
			}
			source = &core.FileWorkSource{WorkID: w.ID, DeliveryID: d.DeliveryID, Version: d.Version, MessageReceipt: d.MessageReceipt}
			b.Inputs = append(append([]core.FileAttachment(nil), b.Inputs...), core.FileAttachment{FileName: d.Name, Data: data})
		}
		realm := ""
		if b.Group {
			realm = h.groupRealm
		}
		var w *work
		for _, old := range st.Works {
			if scope(old.Principal) == scope(b.Principal) && old.SessionID == b.SessionID {
				w = old
				break
			}
		}
		if w != nil {
			if w.Root != b.WorkRoot || w.RootIdentity != identity(rootInfo) || w.InputIdentity != identity(inputInfo) || w.OutputIdentity != identity(outputInfo) || w.OwnerUID != b.OwnerUID || w.GroupRealm != realm {
				return ErrScope
			}
		} else {
			for _, old := range st.Works {
				if old.RootIdentity == identity(rootInfo) || old.Root == b.WorkRoot {
					return ErrScope
				}
			}
			w = &work{ID: uuid.NewString(), SessionID: b.SessionID, Root: b.WorkRoot, RootIdentity: identity(rootInfo), InputIdentity: identity(inputInfo), OutputIdentity: identity(outputInfo), Principal: b.Principal, Route: append(json.RawMessage{}, b.Route...), OwnerUID: b.OwnerUID, Inputs: []Input{}, Deliveries: map[string]*delivery{}, Messages: map[string]bool{}}
			w.GroupRealm = realm
		}
		if !w.Messages[b.Principal.MessageID] {
			if b.DeferInputs && len(w.PendingInputs) >= 128 {
				return ErrUnavailable
			}
			for index, f := range b.Inputs {
				digest := hash(f.Data)
				id := hash([]byte(fmt.Sprintf("%s:%s:%d:%s", w.ID, b.Principal.MessageID, index, digest)))
				name := id + strings.ToLower(filepath.Ext(f.FileName))
				if err := writeNew(h.snapshots, "input_"+name, f.Data, 0400); err != nil {
					return err
				}
				input := Input{ID: id, Name: f.FileName, Path: filepath.Join(b.WorkRoot, "inputs", name), SHA256: digest}
				if source != nil && index == len(b.Inputs)-1 {
					input.Source = source
				}
				if b.DeferInputs {
					if w.PendingInputs == nil {
						w.PendingInputs = map[string][]Input{}
					}
					w.PendingInputs[b.Principal.MessageID] = append(w.PendingInputs[b.Principal.MessageID], input)
				} else {
					if err := writeNew(inputs, name, f.Data, 0440); err != nil {
						return err
					}
					w.Inputs = append(w.Inputs, input)
				}
				result.IncomingInputs = append(result.IncomingInputs, input)
			}
		} else if pending := w.PendingInputs[b.Principal.MessageID]; len(pending) > 0 {
			result.IncomingInputs = append([]Input(nil), pending...)
		}
		w.Messages[b.Principal.MessageID] = true
		if w.GroupRealm != "" {
			// Older binaries discard unknown JSON fields on every ledger write.
			// Mark group provenance so they refuse rather than erase it on rollback.
			st.Schema = 2
		}
		st.Works[w.ID] = w
		incoming := result.IncomingInputs
		result = workContext(w)
		result.IncomingInputs = incoming
		return nil
	})
	return result, err
}

// ActivateInputs publishes only this accepted turn's preserved attachments.
// The caller is the engine at dequeue/reconciliation, never a model tool.
func (h *Host) ActivateInputs(ctx context.Context, p core.ActionPrincipal, workID string) (WorkContext, error) {
	var result WorkContext
	if !validPrincipal(p) {
		return result, ErrInvalid
	}
	err := h.store.change(ctx, func(st *state) error {
		w := st.Works[workID]
		if w == nil || scope(w.Principal) != scope(p) || !w.Messages[p.MessageID] {
			return ErrScope
		}
		if len(w.PendingInputs[p.MessageID]) == 0 {
			result = workContext(w)
			return nil
		}
		r, err := protectedRoot(w.Root)
		if err != nil {
			return err
		}
		defer func() { _ = r.Close() }()
		info, err := r.Stat(".")
		if err != nil || identity(info) != w.RootIdentity {
			return ErrScope
		}
		inputs, info, err := childRoot(r, "inputs", os.Geteuid(), false)
		if err != nil {
			return err
		}
		defer func() { _ = inputs.Close() }()
		if identity(info) != w.InputIdentity {
			return ErrScope
		}
		for _, input := range w.PendingInputs[p.MessageID] {
			name := filepath.Base(input.Path)
			f, err := h.snapshots.Open("input_" + name)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
			_ = f.Close()
			if err != nil || len(data) > MaxFileBytes || hash(data) != input.SHA256 {
				return ErrInvalid
			}
			if info, statErr := inputs.Lstat(name); statErr == nil {
				// A crash may occur after the durable copy but before the ledger
				// commit. Reconcile identical host-owned files without rewriting.
				if !info.Mode().IsRegular() || info.Mode().Perm() != 0440 || checkOwner(info, os.Geteuid(), true) != nil {
					return ErrScope
				}
				f, err := inputs.Open(name)
				if err != nil {
					return err
				}
				existing, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
				_ = f.Close()
				if err != nil || hash(existing) != input.SHA256 {
					return ErrInvalid
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			} else if err := writeNew(inputs, name, data, 0440); err != nil {
				return err
			}
			w.Inputs = append(w.Inputs, input)
		}
		delete(w.PendingInputs, p.MessageID)
		result = workContext(w)
		return nil
	})
	return result, err
}

func (h *Host) FindByMessage(ctx context.Context, p core.ActionPrincipal, messageID string) (WorkRef, error) {
	var result WorkRef
	if !validPrincipal(p) || messageID == "" {
		return result, ErrInvalid
	}
	err := h.store.change(ctx, func(st *state) error {
		for _, w := range st.Works {
			if scope(w.Principal) != scope(p) {
				continue
			}
			matches := w.Messages[messageID]
			for _, input := range w.Inputs {
				matches = matches || (input.Source != nil && input.Source.MessageReceipt == messageID)
			}
			if matches {
				if result.WorkID != "" {
					return ErrScope
				}
				result = WorkRef{WorkID: w.ID, SessionID: w.SessionID}
			}
		}
		if result.WorkID == "" {
			if _, _, err := h.groupArtifact(st, p, messageID); err != nil {
				return err
			}
			result.Import = true
		}
		return nil
	})
	return result, err
}

// Only a confirmed file receipt in the configured Bot/group can cross actor
// boundaries. Original requests and text replies do not identify one artifact.
func (h *Host) groupArtifact(st *state, p core.ActionPrincipal, receipt string) (*work, *delivery, error) {
	var selected *work
	for _, w := range st.Works {
		if w.Messages[receipt] {
			if selected != nil {
				return nil, nil, ErrScope
			}
			selected = w
		}
	}
	if selected == nil {
		return nil, nil, core.ErrFileWorkNotFound
	}
	if h.groupRealm == "" || selected.GroupRealm != h.groupRealm || selected.Principal.Platform != p.Platform || selected.Principal.ChatID != p.ChatID || scope(selected.Principal) == scope(p) {
		return nil, nil, ErrScope
	}
	var result *delivery
	for _, d := range selected.Deliveries {
		if d.Status == "unknown" || d.Status == "submitted" {
			return nil, nil, ErrUncertain
		}
		if d.Status == "accepted" && d.MessageReceipt == receipt {
			if result != nil {
				return nil, nil, ErrScope
			}
			result = d
		}
	}
	if result == nil {
		return nil, nil, core.ErrFileWorkNeedsArtifact
	}
	return selected, result, nil
}

// RecordReply binds a transport-issued text/card receipt to its original work.
// It is not exposed as a model tool and cannot change the work's delivery route.
func (h *Host) RecordReply(ctx context.Context, p core.ActionPrincipal, receipt string) error {
	if !validPrincipal(p) || receipt == "" || len(receipt) > 256 {
		return ErrInvalid
	}
	return h.store.change(ctx, func(st *state) error {
		var selected *work
		for _, w := range st.Works {
			if w.Messages[p.MessageID] {
				if scope(w.Principal) != scope(p) || selected != nil {
					return ErrScope
				}
				selected = w
			}
		}
		if selected == nil {
			return core.ErrFileWorkNotFound
		}
		for _, w := range st.Works {
			if w.ID != selected.ID && w.Messages[receipt] {
				return ErrScope
			}
		}
		selected.Messages[receipt] = true
		return nil
	})
}

type deliverRequest struct {
	WorkID          string `json:"work_id"`
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	ExpectedVersion *int   `json:"expected_version"`
}

func strictInput(raw json.RawMessage, target any) error {
	if len(raw) > 4096 || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return ErrInvalid
	}
	// A duplicate identity/business field must not acquire parser-specific meaning.
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return ErrInvalid
		}
		key, ok := t.(string)
		if !ok || seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			return ErrInvalid
		}
	}
	if _, err := dec.Token(); err != nil {
		return ErrInvalid
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return ErrInvalid
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return ErrInvalid
	}
	return nil
}

func (h *Host) Tool(ctx context.Context, command string, input json.RawMessage, principal core.ActionPrincipal, workID string) (map[string]any, error) {
	if !validPrincipal(principal) || workID == "" {
		return fileRefusal(ErrScope)
	}
	if command == "file-deliver" {
		var request deliverRequest
		if strictInput(input, &request) != nil || request.WorkID != workID || request.ExpectedVersion == nil || *request.ExpectedVersion < 0 || !validHash(request.SHA256) || !relativeFile(request.Path) {
			return fileRefusal(ErrInvalid)
		}
		return h.deliver(ctx, request, principal)
	}
	var result any
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, principal, workID)
		if err != nil {
			return err
		}
		switch command {
		case "work-context":
			if strictInput(input, &struct{}{}) != nil {
				return ErrInvalid
			}
			result = workContext(w)
		case "file-status":
			var r struct {
				WorkID     string `json:"work_id"`
				DeliveryID string `json:"delivery_id"`
			}
			if strictInput(input, &r) != nil || r.WorkID != workID {
				return ErrInvalid
			}
			d := w.Deliveries[r.DeliveryID]
			if d == nil {
				return ErrScope
			}
			result = d.Artifact
		default:
			return ErrInvalid
		}
		return nil
	})
	if err != nil {
		return fileRefusal(err)
	}
	return resultMap(result), nil
}

// Only call before a send attempt, or from a read-only tool. Journal failures
// after submission must stay uncertain even when an underlying error resembles
// a scope/version refusal.
func fileRefusal(err error) (map[string]any, error) {
	for _, known := range []error{ErrScope, ErrInvalid, ErrFormat, ErrVersion, ErrUncertain} {
		if errors.Is(err, known) {
			return map[string]any{"status": "blocked", "code": known.Error()}, err
		}
	}
	return nil, err
}

func owned(st *state, p core.ActionPrincipal, id string) (*work, error) {
	w := st.Works[id]
	if w == nil || scope(w.Principal) != scope(p) {
		return nil, ErrScope
	}
	return w, nil
}

func resultMap(value any) map[string]any {
	data, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	return result
}

func (h *Host) deliver(ctx context.Context, r deliverRequest, p core.ActionPrincipal) (map[string]any, error) {
	var selected Artifact
	var data []byte
	var route json.RawMessage
	created := false
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, r.WorkID)
		if err != nil {
			return err
		}
		requestKey := hash([]byte(fmt.Sprintf("%s:%d:%s:%s", w.ID, *r.ExpectedVersion, r.Path, r.SHA256)))
		for _, d := range w.Deliveries {
			if d.RequestKey == requestKey {
				selected = d.Artifact
				return nil
			}
		}
		if *r.ExpectedVersion != w.Version {
			return ErrVersion
		}
		for _, d := range w.Deliveries {
			if d.Status == "submitted" || d.Status == "unknown" {
				return ErrUncertain
			}
		}
		data, err = readOutput(w, r.Path)
		if err != nil {
			return err
		}
		if hash(data) != r.SHA256 {
			return ErrInvalid
		}
		if err := h.validateFile(ctx, r.Path, data); err != nil {
			if errors.Is(err, ErrUnavailable) {
				return err
			}
			return ErrFormat
		}
		id := uuid.NewString()
		snapshot := id + strings.ToLower(filepath.Ext(r.Path))
		if err := writeNew(h.snapshots, snapshot, data, 0400); err != nil {
			return err
		}
		w.Version++
		selected = Artifact{DeliveryID: id, Version: w.Version, Status: "submitted", Name: filepath.Base(r.Path), Path: r.Path, SHA256: r.SHA256}
		w.Deliveries[id] = &delivery{Artifact: selected, RequestKey: requestKey, Snapshot: snapshot}
		route, created = append(json.RawMessage{}, w.Route...), true
		return nil
	})
	if err != nil {
		return fileRefusal(err)
	}
	if !created {
		return resultMap(selected), nil
	}
	receipt, sendErr := h.send(ctx, route, core.FileAttachment{FileName: selected.Name, MimeType: mimeType(selected.Name), Data: data}, selected.DeliveryID)
	selected.Status = "unknown"
	if sendErr == nil && strings.TrimSpace(receipt) != "" {
		selected.Status, selected.MessageReceipt = "accepted", receipt
	}
	if errors.Is(sendErr, core.ErrFileNotSubmitted) && strings.TrimSpace(receipt) == "" {
		selected.Status = "failed"
	}
	// Persist the send result even when the requesting client has disconnected.
	finish, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = h.store.change(finish, func(st *state) error {
		w, err := owned(st, p, r.WorkID)
		if err != nil {
			return err
		}
		d := w.Deliveries[selected.DeliveryID]
		if d == nil || d.Status != "submitted" {
			return ErrUncertain
		}
		d.Artifact = selected
		if selected.Status == "accepted" {
			w.Messages[receipt] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resultMap(selected), nil
}

func safeName(name string) bool {
	return name != "" && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00\r\n") && name != "." && name != ".."
}

func relativeFile(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsAny(path, "\\:\x00") || !filepath.IsLocal(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func childRoot(root *os.Root, name string, uid int, writable bool) (*os.Root, os.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (checkOwner(info, uid, false) != nil && (!writable || checkOwner(info, os.Geteuid(), false) != nil)) || (!writable && info.Mode().Perm()&0022 != 0) || info.Mode().Perm()&0002 != 0 {
		return nil, nil, ErrInvalid
	}
	r, err := root.OpenRoot(name)
	if err != nil {
		return nil, nil, ErrInvalid
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(info, after) {
		_ = r.Close()
		return nil, nil, ErrInvalid
	}
	return r, after, nil
}

func readOutput(w *work, path string) ([]byte, error) {
	r, err := protectedRoot(w.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	info, err := r.Stat(".")
	if err != nil || identity(info) != w.RootIdentity {
		return nil, ErrScope
	}
	current, outputInfo, err := childRoot(r, "outputs", w.OwnerUID, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = current.Close() }()
	if identity(outputInfo) != w.OutputIdentity {
		return nil, ErrScope
	}
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		next, _, err := childRoot(current, part, w.OwnerUID, true)
		if err != nil {
			return nil, err
		}
		_ = current.Close()
		current = next
	}
	name := parts[len(parts)-1]
	before, err := current.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || checkOwner(before, w.OwnerUID, true) != nil || before.Size() <= 0 || before.Size() > MaxFileBytes {
		return nil, ErrInvalid
	}
	f, err := current.OpenFile(name, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return nil, ErrInvalid
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !unchanged(before, opened) || checkOwner(opened, w.OwnerUID, true) != nil {
		return nil, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || len(data) > MaxFileBytes || int64(len(data)) != before.Size() || !unchanged(opened, after) {
		return nil, ErrInvalid
	}
	return data, nil
}

func writeNew(root *os.Root, name string, data []byte, mode os.FileMode) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return ErrUnavailable
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode) // The service's 0077 umask must not hide selected inputs from its model.
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	dir, err := root.Open(".")
	if err != nil {
		return ErrUnavailable
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil || closeErr != nil {
		return ErrUnavailable
	}
	return nil
}
