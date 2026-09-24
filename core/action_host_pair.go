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
	if err := ValidateActionHosts(h); err != nil {
		return nil, err
	}
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
	if ValidateActionHosts(host) != nil {
		return nil
	}
	return findActionKind(host, kind)
}

func findActionKind(host ActionHost, kind string) ActionHost {
	if pair, ok := host.(*actionHostPair); ok {
		if found := findActionKind(pair.additional, kind); found != nil {
			return found
		}
		return findActionKind(pair.ActionHost, kind)
	}
	if host != nil && host.Kind() == kind {
		return host
	}
	return nil
}

func actionHostForCommand(host ActionHost, command string) ActionHost {
	if ValidateActionHosts(host) != nil {
		return nil
	}
	return findActionCommand(host, command)
}

func findActionCommand(host ActionHost, command string) ActionHost {
	if pair, ok := host.(*actionHostPair); ok {
		for _, candidate := range pair.commands {
			if command == candidate {
				return findActionCommand(pair.additional, command)
			}
		}
		return findActionCommand(pair.ActionHost, command)
	}
	return host
}

// ValidateActionHosts rejects ambiguous composition before any tool/card can
// reach a backend. Primary legacy commands remain reserved for the base host.
func ValidateActionHosts(host ActionHost) error {
	kinds := map[string]bool{}
	commands := map[string]bool{}
	for _, command := range []string{"open", "customer", "customers", "stage", "result", "assignee", "stage-customer-create", "stage-customer-update", "case-list", "case-read", "case-update"} {
		commands[command] = true
	}
	var visit func(ActionHost) error
	visit = func(node ActionHost) error {
		if node == nil {
			return nil
		}
		if pair, ok := node.(*actionHostPair); ok {
			for _, command := range pair.commands {
				if command == "" || commands[command] {
					return errors.New("duplicate action command")
				}
				commands[command] = true
			}
			if err := visit(pair.ActionHost); err != nil {
				return err
			}
			return visit(pair.additional)
		}
		if node.Kind() == "" || kinds[node.Kind()] {
			return errors.New("duplicate action domain")
		}
		kinds[node.Kind()] = true
		return nil
	}
	return visit(host)
}
