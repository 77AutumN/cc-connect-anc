package core

import "errors"

// CombineActionHosts adds a second independent action domain to one session.
// Commands are selected by the application, never by a model-supplied kind.
func CombineActionHosts(primary, additional ActionHost, commands []string) ActionHost {
	if primary == nil {
		return additional
	}
	if additional == nil {
		return primary
	}
	return &actionHostPair{ActionHost: primary, additional: additional, commands: append([]string(nil), commands...)}
}

type actionHostPair struct {
	ActionHost
	additional ActionHost
	commands   []string
}

func (h *actionHostPair) Match(event Event) (ActionRef, bool) {
	if ref, ok := h.additional.Match(event); ok {
		return ref, true
	}
	return h.ActionHost.Match(event)
}

func (h *actionHostPair) SessionEnv(token string) ([]string, error) {
	first, err := h.ActionHost.SessionEnv(token)
	if err != nil {
		return nil, err
	}
	second, err := h.additional.SessionEnv(token)
	if err != nil {
		return nil, err
	}
	if h.Kind() == h.additional.Kind() {
		return nil, errors.New("duplicate action domain")
	}
	return append(first, second...), nil
}

func actionHostForKind(host ActionHost, kind string) ActionHost {
	if pair, ok := host.(*actionHostPair); ok {
		if pair.additional.Kind() == kind {
			return pair.additional
		}
		host = pair.ActionHost
	}
	if host != nil && host.Kind() == kind {
		return host
	}
	return nil
}

func actionHostForCommand(host ActionHost, command string) ActionHost {
	if pair, ok := host.(*actionHostPair); ok {
		for _, candidate := range pair.commands {
			if command == candidate {
				return pair.additional
			}
		}
		return pair.ActionHost
	}
	return host
}
