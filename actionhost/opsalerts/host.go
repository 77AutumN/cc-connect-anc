package opsalerts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

type Category string

const (
	Storage    Category = "storage_unavailable"
	Route      Category = "identity_route_changed"
	Permission Category = "permission_or_route"
	Delivery   Category = "acceptance_unconfirmed"
)

// Fault contains no body, recipient or raw error. Scope is a host-generated hash.
type Fault struct {
	Category Category
	Scope    string
	Count    int
	Since    int64
}
type Snapshot struct {
	Faults   []Fault
	Complete bool
}
type Recipient struct {
	Binding, Chat string
	Sender        core.ReminderSender
}
type event struct {
	ID, Key           string
	Binding, Chat     string
	Category          Category
	First, Last, Next int64
	Count             int
	Closed            bool
	Notified          bool
}
type message struct {
	ID, Binding, Chat, Text, UUID, Receipt string
	Event                                  string
	Order                                  int
	Blocked                                bool
	First, Next, Lease                     int64
	Attempts                               int
	Duplicate                              bool
}

type Host struct {
	store        *Store
	recipient    func() Recipient
	snapshot     func(context.Context) Snapshot
	now          func() time.Time
	lang         core.Language
	mu           sync.Mutex // Tick has one claimant, including the degraded path.
	degradedSent int
	degradedNext int64
}

func New(store *Store, recipient func() Recipient, snapshot func(context.Context) Snapshot, lang core.Language) *Host {
	return &Host{store: store, recipient: recipient, snapshot: snapshot, now: time.Now, lang: lang}
}

func known(c Category) bool     { return c == Storage || c == Route || c == Permission || c == Delivery }
func Scope(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }

func (h *Host) reconcile(st *state, snap Snapshot, r Recipient) {
	now := h.now().Unix()
	seen := map[string]bool{}
	for _, f := range snap.Faults {
		if !known(f.Category) || f.Scope == "" || f.Count < 1 {
			continue
		}
		key := string(f.Category) + ":" + f.Scope
		seen[key] = true
		e := st.Events[key]
		if e == nil || e.Closed {
			first := f.Since
			if first <= 0 || first > now {
				first = now
			}
			next := first
			if f.Category == Delivery {
				next += 15 * 60
			}
			e = &event{ID: "alert_" + uuid.NewString(), Key: key, Binding: r.Binding, Chat: r.Chat, Category: f.Category, First: first, Next: next}
			st.Events[key] = e
		}
		e.Last, e.Count = now, f.Count
		if now >= e.Next && r.Binding != "" && r.Chat != "" && e.Binding == r.Binding && e.Chat == r.Chat {
			h.queue(st, e, r, false)
			e.Notified = true
			e.Next = now + 3600
		}
	}
	// An unreadable reminder DB is not evidence that other failures recovered.
	if !snap.Complete {
		return
	}
	for key, e := range st.Events {
		if e.Closed || seen[key] {
			continue
		}
		if e.Notified {
			if r.Binding == "" || r.Chat == "" || e.Binding != r.Binding || e.Chat != r.Chat {
				continue
			}
			h.queue(st, e, r, true)
		}
		e.Closed = true
	}
}

func (h *Host) queue(st *state, e *event, r Recipient, recovered bool) {
	// Freeze the first warning; merge observations instead of accumulating
	// hourly messages while its acceptance is unknown or permission is denied.
	if !recovered {
		for _, m := range st.Messages {
			if m.Event == e.ID && m.Receipt == "" {
				return
			}
		}
	}
	i18n := core.NewI18n(h.lang)
	status := i18n.T(core.MsgOpsAlertActive)
	if recovered {
		status = i18n.T(core.MsgOpsAlertRecovered)
	}
	label := map[Category]core.MsgKey{Storage: core.MsgOpsAlertStorage, Route: core.MsgOpsAlertRoute, Permission: core.MsgOpsAlertPermission, Delivery: core.MsgOpsAlertDelivery}[e.Category]
	zone := time.FixedZone("Beijing", 8*3600)
	stamp := func(t int64) string { return time.Unix(t, 0).In(zone).Format("2006-01-02 15:04:05 +08:00") }
	text := i18n.Tf(core.MsgOpsAlertBody, status, i18n.T(label), stamp(e.First), stamp(e.Last), e.Count, e.ID)
	id := uuid.NewString()
	st.Messages[id] = &message{ID: id, Event: e.ID, Order: len(st.Messages) + 1, UUID: uuid.NewString(), Binding: r.Binding, Chat: r.Chat, Text: text, Next: h.now().Unix()}
}

func (h *Host) claim(st *state, r Recipient) *message {
	if r.Sender == nil || r.Binding == "" || r.Chat == "" {
		return nil
	}
	items := []*message{}
	for _, m := range st.Messages {
		items = append(items, m)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Order < items[j].Order })
	now := h.now().Unix()
	blockedEvents := map[string]bool{}
	for _, m := range items {
		if m.Receipt != "" {
			continue
		}
		if blockedEvents[m.Event] {
			continue
		}
		blockedEvents[m.Event] = true
		if m.Blocked || m.Next > now || m.Lease > now || m.Binding != r.Binding || m.Chat != r.Chat {
			continue
		}
		if m.First != 0 && now-m.First >= 3600 {
			if !m.Duplicate {
				m.Text = core.NewI18n(h.lang).T(core.MsgReminderDuplicate) + "\n" + m.Text
				m.Duplicate = true
			}
			m.UUID = uuid.NewString()
			m.First = now
		}
		if m.First == 0 {
			m.First = now
		}
		m.Lease = now + 60
		m.Attempts++
		copy := *m
		return &copy
	}
	return nil
}

// Tick uses its own deadline, not reminder attempts (which may be 21 min apart).
func (h *Host) Tick(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.recipient()
	snap := h.snapshot(ctx)
	if h.store == nil {
		h.degraded(ctx, r)
		return
	}
	var m *message
	err := h.store.change(ctx, func(st *state) error { h.reconcile(st, snap, r); m = h.claim(st, r); return nil })
	if err != nil {
		if ctx.Err() == nil {
			h.degraded(ctx, r)
		}
		return
	}
	if m == nil {
		return
	}
	// Revalidate immediately before sending, including after a blocking store call.
	live := h.recipient()
	if live.Sender == nil || live.Binding != m.Binding || live.Chat != m.Chat {
		return
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	receipt, sendErr := live.Sender.SendReminder(c, m.Chat, m.Text, m.UUID)
	cancel()
	finish, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	err = h.store.change(finish, func(st *state) error {
		current := st.Messages[m.ID]
		if current == nil || current.UUID != m.UUID || current.Attempts != m.Attempts {
			return nil
		}
		current.Lease = 0
		if sendErr == nil && receipt != "" {
			current.Receipt = receipt
		} else if errors.Is(sendErr, core.ErrReminderPermission) {
			current.Blocked = true
		} else {
			current.Next = h.now().Add(retryDelay(m.Attempts)).Unix()
		}
		return nil
	})
	if err != nil {
		h.degraded(ctx, h.recipient())
	}
}

func retryDelay(attempt int) time.Duration {
	steps := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	if attempt > 0 && attempt <= len(steps) {
		return steps[attempt-1]
	}
	return time.Hour
}

func (h *Host) degraded(ctx context.Context, r Recipient) {
	now := h.now().Unix()
	if now < h.degradedNext || h.degradedSent >= 3 {
		return
	}
	h.degradedNext = now + 3600
	slog.Error("operations alerts degraded", "code", "alert_storage_unavailable")
	if r.Sender == nil || r.Binding == "" || r.Chat == "" {
		return
	}
	h.degradedSent++
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := r.Sender.SendReminder(c, r.Chat, core.NewI18n(h.lang).T(core.MsgOpsAlertDegraded), uuid.NewString())
	if err != nil {
		slog.Warn("operations alert unconfirmed", "code", "degraded_send_failed")
	}
}

func (h *Host) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Tick(ctx)
		}
	}
}
