package reminders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

const pageRunes = 6000

func reminderDate(at int64, lang core.Language) string {
	t := time.Unix(at, 0).In(beijing)
	day := strings.Split(core.NewI18n(lang).T(core.MsgReminderWeekdays), "|")[int(t.Weekday())]
	return fmt.Sprintf("%s (%s) %s +08:00", t.Format("2006-01-02"), day, t.Format("15:04"))
}

func statusIndex(status string) int {
	switch status {
	case "sending":
		return 1
	case "retry":
		return 2
	case "paused":
		return 3
	case "cancelled":
		return 4
	case "sent":
		return 5
	default:
		return 0
	}
}

func (h *Host) reminderLine(r *Reminder, lang core.Language) string {
	line := fmt.Sprintf("%s | %s\n%s", r.ID, reminderDate(r.At, lang), r.Content)
	if h.now().Unix()-r.At >= 60 {
		return core.NewI18n(lang).T(core.MsgReminderLate) + "\n" + line
	}
	return line
}

func (h *Host) queueList(st *state, r Route, items []*Reminder, lang core.Language) {
	header := core.NewI18n(lang).T(core.MsgReminderList)
	lines := []string{}
	for _, item := range items {
		status := strings.Split(core.NewI18n(lang).T(core.MsgReminderStatuses), "|")[statusIndex(item.Status)]
		lines = append(lines, fmt.Sprintf("%s | %s | v%d | %s\n%s", item.ID, reminderDate(item.At, lang), item.Version, status, item.Content))
	}
	for _, text := range pages(header, lines) {
		h.addBatch(st, r.User, r.PrivateChat, nil, text)
	}
}

func pages(header string, lines []string) []string {
	result := []string{}
	page := header
	for _, line := range lines {
		if utf8.RuneCountInString(page)+utf8.RuneCountInString(line)+2 > pageRunes {
			result = append(result, page)
			page = header
		}
		page += "\n\n" + line
	}
	return append(result, page)
}

func (h *Host) addBatch(st *state, user, destination string, ids []string, text string) *Batch {
	id := uuid.NewString()
	b := &Batch{ID: id, User: user, Destination: destination, Items: ids, Text: text, UUID: uuid.NewString(), Status: "pending", Next: h.now().Unix()}
	st.Batches[id] = b
	for _, id := range ids {
		st.Items[id].Batch = b.ID
		st.Items[id].Status = "sending"
	}
	return b
}

func (h *Host) privateRoute(user string) string {
	if !h.authorized(user) {
		return ""
	}
	for _, r := range h.routes {
		if r.User == user {
			return r.PrivateChat
		}
	}
	return ""
}

func (h *Host) collectDue(st *state, lang core.Language) {
	byUser := map[string][]*Reminder{}
	for _, r := range st.Items {
		if r.Status == "pending" && r.At <= h.now().Unix() {
			byUser[r.User] = append(byUser[r.User], r)
		}
	}
	i18n := core.NewI18n(lang)
	for user, items := range byUser {
		sort.Slice(items, func(i, j int) bool {
			if items[i].At == items[j].At {
				return items[i].ID < items[j].ID
			}
			return items[i].At < items[j].At
		})
		header := i18n.T(core.MsgReminderHeading)
		text := header
		ids := []string{}
		destination := ""
		flush := func() {
			if len(ids) > 0 {
				h.addBatch(st, user, destination, ids, text)
				ids = nil
				text = header
			}
		}
		for _, r := range items {
			if r.Destination != h.privateRoute(user) || h.senders[user] == nil {
				r.Status = "paused"
				slog.Warn("reminder paused", "code", "identity_route_changed")
				continue
			}
			destination = r.Destination
			line := h.reminderLine(r, lang)
			if utf8.RuneCountInString(text)+utf8.RuneCountInString(line)+2 > pageRunes {
				flush()
			}
			text += "\n\n" + line
			ids = append(ids, r.ID)
		}
		flush()
	}
}

// Tick claims one frozen batch. A lease covers process crashes, and the stable
// UUID covers uncertain acceptance. Success means API accepted, not user read.
func (h *Host) Tick(ctx context.Context, lang core.Language) error {
	if h.store == nil {
		return ErrUnavailable
	}
	var claimed *Batch
	err := h.store.change(ctx, func(st *state) error {
		h.collectDue(st, lang)
		claimed = h.claimBatch(st, lang)
		return nil
	})
	if err != nil || claimed == nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	receipt, sendErr := h.senders[claimed.User].SendReminder(sendCtx, claimed.Destination, claimed.Text, claimed.UUID)
	cancel()
	return h.finishBatch(claimed, receipt, sendErr)
}

func (h *Host) claimBatch(st *state, lang core.Language) *Batch {
	ordered := []*Batch{}
	for _, b := range st.Batches {
		ordered = append(ordered, b)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Next == ordered[j].Next {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Next < ordered[j].Next
	})
	for _, b := range ordered {
		if (b.Status != "pending" && b.Status != "retry" && b.Status != "sending") || b.Next > h.now().Unix() || b.LeaseUntil > h.now().Unix() {
			continue
		}
		if h.privateRoute(b.User) != b.Destination || h.senders[b.User] == nil {
			pause(st, b)
			continue
		}
		active := []string{}
		for _, id := range b.Items {
			if st.Items[id] != nil && st.Items[id].Status != "cancelled" {
				active = append(active, id)
			}
		}
		if len(active) != len(b.Items) {
			// Never retry a frozen body containing a cancelled reminder. Survivors
			// get a new persisted batch, marked possibly duplicate if attempted.
			b.Status = "cancelled"
			if len(active) > 0 {
				lines := []string{}
				for _, id := range active {
					r := st.Items[id]
					lines = append(lines, h.reminderLine(r, lang))
				}
				text := core.NewI18n(lang).T(core.MsgReminderHeading) + "\n\n" + strings.Join(lines, "\n\n")
				if b.Attempts > 0 {
					text = core.NewI18n(lang).T(core.MsgReminderDuplicate) + "\n" + text
				}
				h.addBatch(st, b.User, b.Destination, active, text)
			}
			continue
		}
		if b.FirstAttempt != 0 && h.now().Unix()-b.FirstAttempt >= 3600 {
			if !b.PossibleDuplicate {
				b.Text = core.NewI18n(lang).T(core.MsgReminderDuplicate) + "\n" + b.Text
				b.PossibleDuplicate = true
			}
			b.UUID = uuid.NewString()
			b.FirstAttempt = h.now().Unix()
		}
		if b.FirstAttempt == 0 {
			b.FirstAttempt = h.now().Unix()
		}
		b.Attempts++
		b.Status = "sending"
		b.LeaseUntil = h.now().Add(time.Minute).Unix()
		copy := *b
		return &copy
	}
	return nil
}

func (h *Host) finishBatch(claimed *Batch, receipt string, sendErr error) error {
	// Persist an accepted receipt even if the caller's context was cancelled.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	return h.store.change(finishCtx, func(st *state) error {
		b := st.Batches[claimed.ID]
		if b == nil || b.UUID != claimed.UUID || b.Attempts != claimed.Attempts {
			return errors.New("delivery claim changed")
		}
		b.LeaseUntil = 0
		if sendErr == nil && receipt != "" {
			b.Status = "sent"
			b.Receipt = receipt
			for _, id := range b.Items {
				r := st.Items[id]
				r.Receipt = receipt
				if r.Status != "cancelled" {
					r.Status = "sent"
				}
			}
		} else if errors.Is(sendErr, core.ErrReminderPermission) {
			pause(st, b)
		} else {
			b.Status = "retry"
			b.Next = h.now().Add(backoff(b.Attempts)).Unix()
			for _, id := range b.Items {
				if st.Items[id].Status != "cancelled" {
					st.Items[id].Status = "retry"
				}
			}
			slog.Warn("reminder delivery deferred", "code", "acceptance_unconfirmed")
		}
		return nil
	})
}

func pause(st *state, b *Batch) {
	b.Status = "paused"
	b.LeaseUntil = 0
	for _, id := range b.Items {
		if st.Items[id].Status != "cancelled" {
			st.Items[id].Status = "paused"
		}
	}
	slog.Warn("reminder paused", "code", "permission_or_route")
}
func backoff(attempt int) time.Duration {
	minutes := []int{1, 5, 15, 30}
	if attempt > 0 && attempt <= len(minutes) {
		return time.Duration(minutes[attempt-1]) * time.Minute
	}
	return time.Hour
}

// Run is tied to host lifecycle, not any interactive agent session.
func (h *Host) Run(ctx context.Context, lang core.Language) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.Tick(ctx, lang); err != nil {
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, ErrUnavailable) {
					slog.Error("reminders disabled", "code", "storage_unavailable")
					return
				}
				// An interrupted receipt write or a competing process leaves a
				// durable claim. Its lease/UUID permits recovery on a later tick.
				slog.Warn("reminder delivery deferred", "code", "claim_interrupted")
			}
		}
	}
}
