package reminders

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

// CRMPlan comes only from the CRM receipt transaction, never a model request.
type CRMPlan struct {
	EventID       string               `json:"event_id"`
	Namespace     string               `json:"namespace"`
	CustomerID    string               `json:"customer_id"`
	Revision      int                  `json:"revision"`
	ChangeID      string               `json:"change_id"`
	Principal     core.ActionPrincipal `json:"principal"`
	At            string               `json:"at"`
	Content       string               `json:"content"`
	Enabled       bool                 `json:"enabled"`
	CustomerQuery string               `json:"customer_query"`
	CustomerName  string               `json:"customer_name"`
	Binding       CRMRoute             `json:"reminder_binding"`
}

type CRMRoute struct {
	User    string `json:"user_id"`
	Chat    string `json:"source_chat_id"`
	Project string `json:"project"`
	Private string `json:"private_chat_id"`
}

// Disabled is notification state for this revision. Outbox replay cannot undo
// cancellation; only a later explicitly approved plan revision replaces it.
type CRMPlanState struct {
	Plan       CRMPlan `json:"plan"`
	ReminderID string  `json:"reminder_id,omitempty"`
	Disabled   bool    `json:"disabled"`
}

func (p CRMPlan) key() string { return p.Namespace + "\x00" + p.CustomerID }

func validCRMPlan(p CRMPlan) bool {
	for _, value := range []string{p.EventID, p.Namespace, p.CustomerID, p.ChangeID, p.CustomerQuery, p.Principal.UserID, p.Principal.ChatID, p.Principal.Project, p.Principal.SessionKey} {
		if value == "" || len(value) > 1024 || strings.ContainsRune(value, '\x00') {
			return false
		}
	}
	if p.Revision < 1 || p.Principal.Platform != "feishu" {
		return false
	}
	if !p.Enabled {
		return true
	}
	at, err := time.Parse(time.RFC3339, p.At)
	return err == nil && at.Unix() > 0 && strings.TrimSpace(p.Content) != "" && utf8.ValidString(p.Content+p.CustomerName) && utf8.RuneCountInString(p.Content+p.CustomerName) <= 2000 &&
		p.Binding.User == p.Principal.UserID && p.Binding.Chat == p.Principal.ChatID && p.Binding.Project == p.Principal.Project && p.Binding.Private != ""
}

// EnableCRM is startup wiring, absent by default. It reuses the scheduler and
// protected store; the callbacks use the existing authenticated CRM subprocess.
func (h *Host) EnableCRM(call func(context.Context, string, string, map[string]any) (map[string]any, error), stage func(context.Context, CRMPlan, map[string]any, core.ActionPrincipal, string, core.Language) (map[string]any, *core.ActionHostResult, error)) {
	h.crmCall, h.crmStage = call, stage
	h.crmProjects = nil
	for project := range h.routes {
		h.crmProjects = append(h.crmProjects, project)
	}
	sort.Strings(h.crmProjects)
}

func (h *Host) PlanBinding(p core.ActionPrincipal) map[string]any {
	r, ok := h.routes[p.Project]
	if !ok || p.Platform != "feishu" || p.UserID != r.User || p.ChatID != r.Chat || !h.authorized(r.User) || !h.sourceAuthorized(p.Project, r.User) {
		return nil
	}
	return map[string]any{"user_id": r.User, "source_chat_id": r.Chat, "project": r.Project, "private_chat_id": r.PrivateChat}
}

func (h *Host) crmRouteCurrent(p CRMPlan) bool {
	r, ok := h.routes[p.Principal.Project]
	return ok && p.Principal.UserID == r.User && p.Principal.ChatID == r.Chat && p.Binding == (CRMRoute{r.User, r.Chat, r.Project, r.PrivateChat}) && h.authorized(r.User) && h.sourceAuthorized(r.Project, r.User)
}

func decodeCRMPlan(value any) (CRMPlan, error) {
	var plan CRMPlan
	raw, err := json.Marshal(value)
	if err == nil {
		err = json.Unmarshal(raw, &plan)
	}
	if err != nil || !validCRMPlan(plan) {
		return plan, errors.New("invalid_crm_plan")
	}
	return plan, nil
}

func (h *Host) applyCRMPlan(ctx context.Context, plan CRMPlan, lang core.Language) error {
	if !validCRMPlan(plan) {
		return errors.New("invalid_crm_plan")
	}
	return h.store.change(ctx, func(st *state) error {
		// Old binaries must fail closed, not discard CRM links as unknown JSON.
		st.Schema = 2
		if st.Plans == nil {
			st.Plans = map[string]*CRMPlanState{}
		}
		previous := st.Plans[plan.key()]
		if previous != nil && previous.Plan.Revision >= plan.Revision {
			if previous.Plan.Revision == plan.Revision && previous.Plan != plan {
				return errors.New("crm_revision_conflict")
			}
			return nil
		}
		if previous != nil && previous.ReminderID != "" {
			old := st.Items[previous.ReminderID]
			if old.Status != "sent" && old.Status != "cancelled" {
				old.Status = "cancelled"
				old.Version++
			}
		}
		current := &CRMPlanState{Plan: plan, Disabled: !plan.Enabled}
		st.Plans[plan.key()] = current
		if !plan.Enabled {
			return nil
		}
		at, _ := time.Parse(time.RFC3339, plan.At)
		item := &Reminder{ID: "rem_" + uuid.NewString(), User: plan.Principal.UserID, Chat: plan.Principal.ChatID, Destination: plan.Binding.Private, Content: core.NewI18n(lang).Tf(core.MsgCRMReminderContent, plan.CustomerName, plan.Content), At: at.Unix(), Version: 1, Status: "pending", CRM: &plan}
		if !h.crmRouteCurrent(plan) {
			item.Status = "paused"
			item.PauseReason = "crm_plan_unverified"
		}
		st.Items[item.ID], current.ReminderID = item, item.ID
		return nil
	})
}

// One source per tick bounds a failed CRM helper's impact on ordinary delivery.
// Repeated events are harmless; acknowledgements happen only after store commit.
func (h *Host) syncCRM(ctx context.Context, lang core.Language) {
	if h.store == nil || h.crmCall == nil || len(h.crmProjects) == 0 {
		return
	}
	project := h.crmProjects[(h.crmCursor.Add(1)-1)%uint64(len(h.crmProjects))]
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := h.consumeCRM(ctx, project, lang)
	if err != nil {
		h.crmFaults.Store(project, true)
		slog.Warn("CRM reminder synchronization paused", "code", "crm_plan_sync_unavailable")
	} else {
		h.crmFaults.Delete(project)
	}
}

func (h *Host) consumeCRM(ctx context.Context, project string, lang core.Language) error {
	data, err := h.crmCall(ctx, project, "host-plan-events", map[string]any{"limit": 20})
	if err != nil || data["status"] != "ok" {
		return errors.New("crm_plan_events_unavailable")
	}
	events, ok := data["events"].([]any)
	if !ok || len(events) > 100 {
		return errors.New("invalid_crm_plan_events")
	}
	for _, raw := range events {
		plan, err := decodeCRMPlan(raw)
		if err != nil {
			return err
		}
		if err = h.applyCRMPlan(ctx, plan, lang); err != nil {
			return err
		}
		ack, err := h.crmCall(ctx, project, "host-plan-ack", map[string]any{"event_id": plan.EventID})
		if err != nil || ack["status"] != "acknowledged" {
			return errors.New("crm_plan_ack_unavailable")
		}
	}
	return nil
}

func (h *Host) checkCRM(ctx context.Context, plan CRMPlan) bool {
	return h.checkCRMAt(ctx, plan, plan.Principal.Project)
}

func (h *Host) checkCRMAt(ctx context.Context, plan CRMPlan, project string) bool {
	if h.crmCall == nil || !h.crmRouteCurrent(plan) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := h.crmCall(ctx, project, "host-plan-read", map[string]any{"namespace": plan.Namespace, "customer_id": plan.CustomerID, "revision": plan.Revision})
	if err != nil || data["status"] != "current" {
		return false
	}
	current, err := decodeCRMPlan(data["plan"])
	return err == nil && current == plan && current.Enabled
}

// Checks run outside the database transaction. The final claim rechecks all
// local versions, cancellations and routes after these bounded external reads.
func (h *Host) checkClaimCRM(ctx context.Context, claimed *Batch) (bool, error) {
	var plan *CRMPlan
	err := h.store.change(ctx, func(st *state) error {
		for _, id := range claimed.Items {
			if item := st.Items[id]; item != nil && item.CRM != nil {
				copy := *item.CRM
				plan = &copy
			}
		}
		return nil
	})
	if err != nil || plan == nil {
		return err == nil, err
	}
	verified := h.checkCRM(ctx, *plan)
	ready := false
	err = h.store.change(ctx, func(st *state) error {
		batch := st.Batches[claimed.ID]
		if batch == nil || batch.UUID != claimed.UUID || batch.Attempts != claimed.Attempts {
			return errors.New("delivery_claim_changed")
		}
		item := st.Items[claimed.Items[0]]
		current := st.Plans[plan.key()]
		ready = len(claimed.Items) == 1 && verified && current != nil && current.Plan == *plan && !current.Disabled && current.ReminderID == item.ID && item.Status != "cancelled" && item.Status != "paused" && h.crmRouteCurrent(*plan)
		if !ready {
			batch.Status, batch.LeaseUntil = "paused", 0
			if item.Status != "cancelled" && item.Status != "sent" {
				item.Status = "paused"
				item.PauseReason = "crm_plan_unverified"
				item.Version++
			}
		}
		return nil
	})
	return ready, err
}

func (h *Host) updateCRM(ctx context.Context, route Route, in input, principal core.ActionPrincipal, token string, lang core.Language) (map[string]any, *core.ActionHostResult, bool, error) {
	var plan *CRMPlan
	var failure map[string]any
	err := h.store.change(ctx, func(st *state) error {
		item := st.Items[*in.ID]
		if item == nil || item.CRM == nil {
			return nil
		}
		copy := *item.CRM
		plan = &copy
		if item.User != route.User || (route.Chat != route.PrivateChat && item.Chat != route.Chat) {
			failure = blocked("not_found")
		} else if item.Version != *in.Version {
			failure = blocked("version_conflict")
		} else if current := st.Plans[plan.key()]; current == nil || current.Plan.Revision != plan.Revision || current.Disabled || item.Status == "cancelled" || item.Status == "sent" {
			failure = blocked("already_final")
		}
		return nil
	})
	if err != nil || plan == nil {
		return nil, nil, plan != nil, err
	}
	if failure != nil {
		return failure, nil, true, nil
	}
	if h.crmStage == nil || !h.checkCRM(ctx, *plan) {
		return blocked("crm_plan_unavailable"), nil, true, nil
	}
	// Private and group projects may point to different CRM environments. A
	// coincidentally equal customer number/revision is not permission to retarget.
	if principal.Project != plan.Principal.Project && !h.checkCRMAt(ctx, *plan, principal.Project) {
		return blocked("crm_plan_environment_changed"), nil, true, nil
	}
	if err = validateInput("reminder-update", in, h.now()); err != nil {
		return blocked(err.Error()), nil, true, nil
	}
	changes := map[string]any{}
	if in.At != nil {
		changes["next_followup_at"] = *in.At
	}
	if in.Content != nil {
		changes["next_action"] = *in.Content
	}
	result, card, err := h.crmStage(ctx, *plan, changes, principal, token, lang)
	return result, card, true, err
}
