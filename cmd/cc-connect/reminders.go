package main

import (
	"context"
	"errors"
	"github.com/chenhg5/cc-connect/actionhost/reminders"
	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type authorizedReminderSender struct {
	engine *core.Engine
	user   string
	sender core.ReminderSender
}

func (s authorizedReminderSender) ReminderAuthorized() bool {
	return s.engine.HostedUserAuthorized(s.user)
}
func (s authorizedReminderSender) SendReminder(ctx context.Context, chat, text, id string) (string, error) {
	if !s.ReminderAuthorized() {
		return "", core.ErrReminderPermission
	}
	return s.sender.SendReminder(ctx, chat, text, id)
}

// The CRM project-set validator has already checked exact identities, one app,
// six isolated environments, three private chats and a shared group. This only
// derives the private mapping; no second identity configuration is introduced.
func reminderRoutes(cfg *config.Config, projects map[string]bool) ([]reminders.Route, error) {
	if len(projects) != 6 {
		return nil, errors.New("reminders require six verified environments")
	}
	counts := map[string]int{}
	for _, p := range cfg.Projects {
		if projects[p.Name] {
			chat, _ := p.Platforms[0].Options["allow_chat"].(string)
			counts[chat]++
		}
	}
	private := map[string]string{}
	for _, p := range cfg.Projects {
		if projects[p.Name] {
			o := p.Platforms[0].Options
			chat, _ := o["allow_chat"].(string)
			user, _ := o["allow_from"].(string)
			if counts[chat] == 1 {
				if private[user] != "" {
					return nil, errors.New("ambiguous private route")
				}
				private[user] = chat
			}
		}
	}
	if len(private) != 3 {
		return nil, errors.New("missing private route")
	}
	result := []reminders.Route{}
	for _, p := range cfg.Projects {
		if projects[p.Name] {
			o := p.Platforms[0].Options
			chat, _ := o["allow_chat"].(string)
			user, _ := o["allow_from"].(string)
			if private[user] == "" {
				return nil, errors.New("missing private route")
			}
			result = append(result, reminders.Route{User: user, Chat: chat, Project: p.Name, PrivateChat: private[user]})
		}
	}
	return result, nil
}
