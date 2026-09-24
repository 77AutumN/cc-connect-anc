package files

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Host-selected receipt, never a model-provided path or another actor's cwd.
// Stage the existing snapshot as an input and reuse normal input activation,
// including crash reconciliation and ordinary-UID directory checks.
func (h *Host) importCaseArtifact(ctx context.Context, p core.ActionPrincipal, workID, receipt string) (map[string]any, error) {
	err := h.store.change(ctx, func(st *state) error {
		w, err := owned(st, p, workID)
		if err != nil {
			return err
		}
		if !w.Messages[p.MessageID] || (h.projectAccess == nil && (w.GroupRealm == "" || w.GroupRealm != h.groupRealm)) {
			return ErrScope
		}
		source, artifact, err := h.sharedArtifact(ctx, st, p, receipt, workID, true)
		if err != nil {
			return err
		}
		if source.ID == w.ID {
			return nil
		}
		for _, input := range append(append([]Input(nil), w.Inputs...), w.PendingInputs[p.MessageID]...) {
			if input.Source != nil && input.Source.MessageReceipt == receipt {
				return nil
			}
		}
		if len(w.Inputs) >= 128 {
			return ErrUnavailable
		}
		id := hash([]byte(w.ID + ":" + artifact.DeliveryID))
		input := Input{ID: id, Name: artifact.Name, Path: filepath.Join(w.Root, "inputs", id+strings.ToLower(filepath.Ext(artifact.Name))), SHA256: artifact.SHA256,
			Source: &core.FileWorkSource{WorkID: source.ID, DeliveryID: artifact.DeliveryID, Version: artifact.Version, MessageReceipt: receipt}}
		if w.PendingInputs == nil {
			w.PendingInputs = map[string][]Input{}
		}
		w.PendingInputs[p.MessageID] = append(w.PendingInputs[p.MessageID], input)
		return nil
	})
	if err != nil {
		return fileRefusal(err)
	}
	work, err := h.ActivateInputs(ctx, p, workID)
	if err != nil {
		return fileRefusal(err)
	}
	return resultMap(work), nil
}
