package reminders

import (
	"context"
	"github.com/chenhg5/cc-connect/actionhost/opsalerts"
)

// HealthSnapshot reads committed states, excluding bodies and identity values.
// Complete=false means an unavailable DB cannot prove other failures recovered.
func (h *Host) HealthSnapshot(ctx context.Context) opsalerts.Snapshot {
	failed := opsalerts.Snapshot{Faults: []opsalerts.Fault{{Category: opsalerts.Storage, Scope: "reminder-store", Count: 1}}}
	if h == nil || h.store == nil {
		return failed
	}
	groups := map[string]*opsalerts.Fault{}
	add := func(category opsalerts.Category, user string, since int64, count int) {
		key := string(category) + ":" + opsalerts.Scope(user)
		f := groups[key]
		if f == nil {
			f = &opsalerts.Fault{Category: category, Scope: opsalerts.Scope(user)}
			groups[key] = f
		}
		f.Count += count
		if since > 0 && (f.Since == 0 || since < f.Since) {
			f.Since = since
		}
	}
	err := h.store.change(ctx, func(st *state) error {
		for _, r := range st.Items {
			if r.Status == "sent" || r.Status == "cancelled" {
				continue
			}
			// Check future reminders too: revocation need not wait for due time.
			if r.Destination != h.privateRoute(r.User) || h.senders[r.User] == nil {
				add(opsalerts.Route, r.User, 0, 1)
			} else if r.Status == "paused" {
				category := opsalerts.Permission
				if r.PauseReason == "crm_plan_unverified" {
					category = opsalerts.Delivery
				}
				add(category, r.User, 0, 1)
			}
		}
		for _, b := range st.Batches {
			if b.Status == "paused" && len(b.Items) > 0 {
				continue
			}
			// In-flight retries retain the incident until a verified receipt.
			if b.Status == "sending" && b.Attempts < 2 {
				continue
			}
			if b.Status != "pending" && b.Status != "retry" && b.Status != "sending" && b.Status != "paused" {
				continue
			}
			if b.Destination != h.privateRoute(b.User) || h.senders[b.User] == nil {
				if len(b.Items) == 0 {
					add(opsalerts.Route, b.User, 0, 1)
				}
				continue
			}
			if b.Status == "paused" {
				add(opsalerts.Permission, b.User, 0, 1)
			} else if b.Status != "pending" {
				count := 0
				for _, id := range b.Items {
					if r := st.Items[id]; r != nil && r.Status != "cancelled" {
						count++
					}
				}
				if len(b.Items) == 0 {
					count = 1
				} // one private-list page
				if count > 0 {
					add(opsalerts.Delivery, b.User, b.FirstAttempt, count)
				}
			}
		}
		return nil
	})
	if err != nil {
		return failed
	}
	result := opsalerts.Snapshot{Complete: true}
	h.crmFaults.Range(func(project, _ any) bool {
		add(opsalerts.Storage, "crm-plan-sync:"+project.(string), 0, 1)
		return true
	})
	for _, f := range groups {
		result.Faults = append(result.Faults, *f)
	}
	return result
}
