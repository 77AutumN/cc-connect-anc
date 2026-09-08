package feishu

import (
	"errors"
	"github.com/chenhg5/cc-connect/core"
	"log/slog"
	"strings"
	"sync"
)

// sharedWSGroup tracks all Platform instances sharing the same Feishu app
// WebSocket connection. When multiple projects use the same app_id, Feishu's
// server load-balances messages across WebSocket connections. By sharing a
// single connection and fanning out events to all platforms, every project
// receives every message and can apply its own allow_chat / allow_from filters.
type sharedWSGroup struct {
	mu        sync.RWMutex
	platforms []*Platform
}

var (
	sharedWSMu     sync.Mutex
	sharedWSGroups = map[string]*sharedWSGroup{} // key: app_id "|" domain
)

func sharedWSKey(appID, domain string) string {
	return appID + "|" + domain
}

// registerSharedWS registers a platform in the shared WebSocket group for its
// app_id+domain. Returns the group and whether this platform is the primary
// (first to register and responsible for owning the WebSocket connection).
func registerSharedWS(p *Platform) (group *sharedWSGroup, isPrimary bool) {
	key := sharedWSKey(p.appID, p.domain)
	sharedWSMu.Lock()
	defer sharedWSMu.Unlock()

	g, exists := sharedWSGroups[key]
	if !exists {
		g = &sharedWSGroup{}
		sharedWSGroups[key] = g
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, sibling := range g.platforms {
		if sibling == p {
			return g, i == 0
		}
	}
	g.platforms = append(g.platforms, p)
	isPrimary = len(g.platforms) == 1
	if !isPrimary {
		slog.Info("feishu: sharing WebSocket connection",
			"app_id", p.appID, "platforms", len(g.platforms))
	}
	return g, isPrimary
}

// Prepare registers the fixed route without opening a connection. The host
// prepares every environment before starting any receiver.
func (p *Platform) Prepare(handler core.MessageHandler) error {
	if p.strictRoutes {
		for _, value := range []string{p.allowFrom, p.allowChat} {
			if value == "" || strings.ContainsAny(value, "*, \t\r\n") {
				return errors.New("fixed routes require one exact sender and chat")
			}
		}
		if p.shareSessionInChannel || p.threadIsolation || p.shouldUseWebhookMode() {
			return errors.New("fixed routes require isolated sessions and one websocket transport")
		}
	}
	group, primary := registerSharedWS(p)
	for _, sibling := range group.allPlatforms() {
		if sibling != p && (p.strictRoutes || sibling.strictRoutes) &&
			(!p.strictRoutes || !sibling.strictRoutes || (p.allowFrom == sibling.allowFrom && p.allowChat == sibling.allowChat) ||
				p.appSecret != sibling.appSecret) {
			unregisterSharedWS(p)
			return errors.New("ambiguous or inconsistent fixed receiver routes")
		}
	}
	p.sharedGroup, p.isWSPrimary = group, primary
	p.mu.Lock()
	p.handler = handler
	p.mu.Unlock()
	return nil
}

func (p *Platform) acceptFixedRoute(sender, chat string) bool {
	if !p.strictRoutes {
		return true
	}
	if p.sharedGroup == nil || sender == "" || chat == "" {
		return false
	}
	var match *Platform
	for _, sibling := range p.sharedGroup.allPlatforms() {
		if !sibling.strictRoutes {
			return false
		}
		if sibling.allowFrom == sender && sibling.allowChat == chat {
			if match != nil {
				return false
			}
			match = sibling
		}
	}
	return match == p
}

// unregisterSharedWS removes a platform from its shared group.
// Returns the number of platforms remaining in the group.
func unregisterSharedWS(p *Platform) int {
	key := sharedWSKey(p.appID, p.domain)
	sharedWSMu.Lock()
	defer sharedWSMu.Unlock()

	g, exists := sharedWSGroups[key]
	if !exists {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, plat := range g.platforms {
		if plat == p {
			g.platforms = append(g.platforms[:i], g.platforms[i+1:]...)
			break
		}
	}
	remaining := len(g.platforms)
	if remaining == 0 {
		delete(sharedWSGroups, key)
	}
	return remaining
}

// allPlatforms returns a snapshot of all platforms in the group.
func (g *sharedWSGroup) allPlatforms() []*Platform {
	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]*Platform, len(g.platforms))
	copy(result, g.platforms)
	return result
}
