package reminders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

// Route is populated only from the six verified host environments.
type Route struct{ User, Chat, Project, PrivateChat string }
type Sender interface {
	SendReminder(context.Context, string, string, string) (string, error)
}
type Host struct {
	store            *Store
	routes           map[string]Route
	senders          map[string]Sender
	now              func() time.Time
	sourceAuthorized func(project, user string) bool
	crmCall          func(context.Context, string, string, map[string]any) (map[string]any, error)
	crmStage         func(context.Context, CRMPlan, map[string]any, core.ActionPrincipal, string, core.Language) (map[string]any, *core.ActionHostResult, error)
	crmProjects      []string
	crmCursor        atomic.Uint64
	crmFaults        sync.Map
}

func New(store *Store, routes []Route, senders map[string]Sender, sourceAuthorized func(project, user string) bool) (*Host, error) {
	if sourceAuthorized == nil {
		return nil, errors.New("missing source authorization")
	}
	h := &Host{store: store, routes: map[string]Route{}, senders: map[string]Sender{}, now: time.Now, sourceAuthorized: sourceAuthorized}
	private := map[string]string{}
	for _, r := range routes {
		if r.User == "" || r.Chat == "" || r.Project == "" || r.PrivateChat == "" || h.routes[r.Project].Project != "" {
			return nil, errors.New("invalid reminder routes")
		}
		if chat := private[r.User]; chat != "" && chat != r.PrivateChat {
			return nil, errors.New("ambiguous private route")
		}
		for user, chat := range private {
			if user != r.User && chat == r.PrivateChat {
				return nil, errors.New("shared private route")
			}
		}
		private[r.User] = r.PrivateChat
		h.routes[r.Project] = r
	}
	for user, sender := range senders {
		h.senders[user] = sender
	}
	for user := range private {
		if h.senders[user] == nil {
			return nil, errors.New("missing private sender")
		}
	}
	return h, nil
}

func Commands() []string {
	return []string{"reminder-create", "reminder-list", "reminder-update", "reminder-cancel"}
}
func (h *Host) Kind() string                            { return "reminders" }
func (h *Host) RequiresMessageClock() bool              { return true }
func (h *Host) Match(core.Event) (core.ActionRef, bool) { return core.ActionRef{}, false }
func (h *Host) SessionEnv(string) ([]string, error)     { return nil, nil }

// Reminders are directly authorized business commands, not approval cards.
var errNoCards = errors.New("reminders do not accept card actions")

func (h *Host) Begin(context.Context, core.ActionRef, core.ActionPrincipal, string, core.Language) (core.ActionHostResult, error) {
	return core.ActionHostResult{}, errNoCards
}
func (h *Host) BindCard(context.Context, string, core.ActionPrincipal) error { return errNoCards }
func (h *Host) Claim(context.Context, string, core.ActionDecision, core.ActionPrincipal, core.Language) (core.ActionHostResult, bool, error) {
	return core.ActionHostResult{}, false, errNoCards
}
func (h *Host) Execute(context.Context, string, core.ActionPrincipal, core.Language) (core.ActionHostResult, error) {
	return core.ActionHostResult{}, errNoCards
}

type input struct {
	Content *string `json:"content,omitempty"`
	At      *string `json:"at,omitempty"`
	ID      *string `json:"id,omitempty"`
	Version *int    `json:"version,omitempty"`
	// all is a presentation request, never a recipient selector. In a group,
	// it queues a host-to-private list and returns no reminder bodies.
	All *bool `json:"all,omitempty"`
}

func decode(raw json.RawMessage) (input, error) {
	var in input
	var fields map[string]json.RawMessage
	d := json.NewDecoder(bytes.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return in, errors.New("invalid input")
	}
	fields = map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return in, err
		}
		k, ok := key.(string)
		if !ok || fields[k] != nil {
			return in, errors.New("duplicate input")
		}
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return in, err
		}
		if bytes.Equal(v, []byte("null")) {
			return in, errors.New("null field")
		}
		fields[k] = v
	}
	if _, err = d.Token(); err != nil {
		return in, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return in, errors.New("trailing input")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	err = d.Decode(&in)
	return in, err
}

func blocked(code string) map[string]any { return map[string]any{"status": "blocked", "code": code} }

func (h *Host) Tool(ctx context.Context, command string, raw json.RawMessage, p core.ActionPrincipal, token string, lang core.Language) (map[string]any, *core.ActionHostResult, error) {
	r, ok := h.routes[p.Project]
	if !ok || p.Platform != "feishu" || r.User != p.UserID || r.Chat != p.ChatID || p.MessageID == "" || !h.authorized(r.User) || !h.sourceAuthorized(p.Project, p.UserID) {
		return blocked("invalid_identity"), nil, nil
	}
	in, err := decode(raw)
	if err != nil {
		return blocked("invalid_input"), nil, nil
	}
	if err = validateInput(command, in, time.Time{}); err != nil {
		return blocked(err.Error()), nil, nil
	}
	if h.store == nil {
		return nil, nil, ErrUnavailable
	}
	if command == "reminder-update" {
		if result, card, handled, err := h.updateCRM(ctx, r, in, p, token, lang); handled || err != nil {
			return result, card, err
		}
	}
	canonical, _ := json.Marshal(in)
	digest := sha256.Sum256([]byte(p.Project + "\x00" + p.UserID + "\x00" + p.ChatID + "\x00" + p.MessageID + "\x00" + command + "\x00" + string(canonical)))
	key := hex.EncodeToString(digest[:])
	var result map[string]any
	err = h.store.change(ctx, func(st *state) error {
		if command != "reminder-list" || (in.All != nil && *in.All && r.Chat != r.PrivateChat) {
			if cached := st.Requests[key]; cached != nil {
				return json.Unmarshal(cached, &result)
			}
		}
		if err := validateInput(command, in, h.now()); err != nil {
			result = blocked(err.Error())
			return nil
		}
		switch command {
		case "reminder-create":
			at, _ := time.Parse(time.RFC3339, *in.At)
			item := &Reminder{ID: "rem_" + uuid.NewString(), User: r.User, Chat: r.Chat, Destination: r.PrivateChat, Content: strings.TrimSpace(*in.Content), At: at.Unix(), Version: 1, Status: "pending"}
			st.Items[item.ID] = item
			result = map[string]any{"status": "created", "reminder": visible(item)}
		case "reminder-list":
			result = h.list(st, r, in, lang)
		case "reminder-update", "reminder-cancel":
			result = changeReminder(st, r, command, in)
		}
		if result == nil {
			return errors.New("unsupported_command")
		}
		if command != "reminder-list" || (in.All != nil && *in.All && r.Chat != r.PrivateChat) {
			data, err := json.Marshal(result)
			if err != nil {
				return err
			}
			st.Requests[key] = data
		}
		return nil
	})
	return result, nil, err
}

var beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

func changeReminder(st *state, r Route, command string, in input) map[string]any {
	item := st.Items[*in.ID]
	if item == nil || item.User != r.User || (r.Chat != r.PrivateChat && item.Chat != r.Chat) {
		return blocked("not_found")
	}
	if item.Version != *in.Version {
		return blocked("version_conflict")
	}
	if item.Status != "pending" && item.Status != "sending" && item.Status != "retry" && item.Status != "paused" {
		return blocked("already_final")
	}
	if command == "reminder-update" && item.Status != "pending" {
		return blocked("already_dispatching")
	}
	item.Version++
	if command == "reminder-cancel" {
		item.Status = "cancelled"
		if item.CRM != nil {
			plan := st.Plans[item.CRM.key()]
			if plan != nil && plan.Plan.Revision == item.CRM.Revision {
				plan.Disabled = true
			}
		}
		return map[string]any{"status": "cancelled", "id": item.ID, "version": item.Version, "may_be_in_flight": item.Batch != ""}
	}
	if in.At != nil {
		at, _ := time.Parse(time.RFC3339, *in.At)
		item.At = at.Unix()
	}
	if in.Content != nil {
		item.Content = strings.TrimSpace(*in.Content)
	}
	return map[string]any{"status": "updated", "reminder": visible(item)}
}

func (h *Host) authorized(user string) bool {
	sender := h.senders[user]
	if sender == nil {
		return false
	}
	if live, ok := sender.(interface{ ReminderAuthorized() bool }); ok {
		return live.ReminderAuthorized()
	}
	return true
}

func (h *Host) list(st *state, r Route, in input, lang core.Language) map[string]any {
	items := scopedItems(st, r, in.All != nil && *in.All)
	if in.All != nil && *in.All && r.Chat != r.PrivateChat {
		h.queueList(st, r, items, lang)
		return map[string]any{"status": "private_list_queued"}
	}
	if in.ID != nil {
		position := -1
		for i, item := range items {
			if item.ID == *in.ID {
				position = i
				break
			}
		}
		if position < 0 {
			return blocked("invalid_cursor")
		}
		items = items[position+1:]
	}
	next := ""
	if len(items) > 20 {
		items = items[:20]
		next = items[len(items)-1].ID
	}
	rows := []map[string]any{}
	for _, item := range items {
		row := visible(item)
		if hint := crmPauseHint(item, lang); hint != "" {
			row["message"] = hint
		}
		rows = append(rows, row)
	}
	result := map[string]any{"status": "ok", "reminders": rows, "reference_time": h.now().In(beijing).Format(time.RFC3339), "timezone": "Asia/Shanghai"}
	if next != "" {
		result["next_id"] = next
	}
	return result
}

func validateInput(command string, in input, now time.Time) error {
	bad := errors.New("invalid_input")
	switch command {
	case "reminder-create":
		if in.Content == nil || in.At == nil || in.ID != nil || in.Version != nil || in.All != nil {
			return bad
		}
	case "reminder-list":
		if in.Content != nil || in.At != nil || in.Version != nil || (in.All != nil && *in.All && in.ID != nil) {
			return bad
		}
	case "reminder-update":
		if in.ID == nil || in.Version == nil || (in.Content == nil && in.At == nil) || in.All != nil {
			return bad
		}
	case "reminder-cancel":
		if in.ID == nil || in.Version == nil || in.Content != nil || in.At != nil || in.All != nil {
			return bad
		}
	default:
		return bad
	}
	if in.Version != nil && *in.Version < 1 {
		return bad
	}
	if in.Content != nil && (strings.TrimSpace(*in.Content) == "" || utf8.RuneCountInString(*in.Content) > 2000 || !utf8.ValidString(*in.Content)) {
		return bad
	}
	if in.At != nil {
		at, err := time.Parse(time.RFC3339, *in.At)
		if err != nil {
			return errors.New("time_requires_offset")
		}
		if !at.After(now) {
			return errors.New("time_in_past")
		}
	}
	return nil
}

func visible(r *Reminder) map[string]any {
	result := map[string]any{"id": r.ID, "content": r.Content, "at": time.Unix(r.At, 0).In(beijing).Format(time.RFC3339), "weekday": time.Unix(r.At, 0).In(beijing).Weekday().String(), "timezone": "Asia/Shanghai", "version": r.Version, "status": r.Status}
	if r.CRM != nil {
		result["source"] = "crm"
		result["customer_query"] = r.CRM.CustomerQuery
		result["changes_require_crm_approval"] = true
		if r.Status == "paused" && r.PauseReason != "" {
			result["pause_reason"] = r.PauseReason
		}
	}
	return result
}
func scopedItems(st *state, r Route, all bool) []*Reminder {
	items := []*Reminder{}
	for _, item := range st.Items {
		if item.User == r.User && (r.Chat == r.PrivateChat || all || item.Chat == r.Chat) {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].At == items[j].At {
			return items[i].ID < items[j].ID
		}
		return items[i].At < items[j].At
	})
	return items
}
