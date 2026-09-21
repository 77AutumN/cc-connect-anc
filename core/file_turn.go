package core

import (
	"encoding/json"
	"errors"
	"fmt"
)

// FileTurn is a bounded receipt for an accepted supplement, in the existing
// session snapshot. It holds no process handles or model capability tokens.
type FileTurn struct {
	Principal       ActionPrincipal `json:"principal"`
	WorkID          string          `json:"work_id"`
	Content         string          `json:"content"`
	Route           json.RawMessage `json:"route"`
	Inputs          []FileWorkInput `json:"inputs,omitempty"`
	Status          string          `json:"status"` // queued, started, completed, stopped
	ResumeMessageID string          `json:"resume_message_id,omitempty"`
}

const retainedFileTurns = 128

var errFileTurnStorage = errors.New("file supplement could not be saved")

func unfinishedFileTurn(t FileTurn) bool { return t.Status == "queued" || t.Status == "started" }

func cloneFileTurns(turns []FileTurn) []FileTurn {
	copy := append([]FileTurn(nil), turns...)
	for i := range copy {
		copy[i].Route = append(json.RawMessage(nil), copy[i].Route...)
		copy[i].Inputs = append([]FileWorkInput(nil), copy[i].Inputs...)
	}
	return copy
}

func (s *Session) fileTurns() []FileTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneFileTurns(s.FileTurns)
}

func (s *Session) hasUnfinishedFileTurns() bool {
	for _, turn := range s.fileTurns() {
		if unfinishedFileTurn(turn) {
			return true
		}
	}
	return false
}

func validateFileTurns(sessions map[string]*Session) error {
	for _, s := range sessions {
		if s == nil {
			return errFileTurnStorage
		}
		if len(s.FileTurns) > retainedFileTurns {
			return errFileTurnStorage
		}
		seen := map[string]bool{}
		for _, turn := range s.FileTurns {
			p := turn.Principal
			if p.MessageID == "" || p.Platform == "" || p.Project == "" || p.UserID == "" || p.ChatID == "" || p.SessionKey == "" || turn.WorkID == "" || !json.Valid(turn.Route) || seen[p.MessageID] {
				return errFileTurnStorage
			}
			seen[p.MessageID] = true
			switch turn.Status {
			case "queued", "started", "completed", "stopped":
			default:
				return errFileTurnStorage
			}
		}
	}
	return nil
}

// Mutations and writes share the manager lock so an older concurrent save
// cannot overwrite an acknowledgment. On failure keep the prior in-memory state.
func (sm *SessionManager) updateFileTurns(s *Session, update func(*[]FileTurn) error) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.storePath == "" || sm.loadErr != nil || sm.sessions[s.ID] != s {
		return errFileTurnStorage
	}
	s.mu.Lock()
	previous := cloneFileTurns(s.FileTurns)
	err := update(&s.FileTurns)
	s.mu.Unlock()
	if err == nil {
		err = sm.saveLockedError()
	}
	if err != nil {
		s.mu.Lock()
		s.FileTurns = previous
		s.mu.Unlock()
		return fmt.Errorf("save file supplement: %w", err)
	}
	return nil
}

func (sm *SessionManager) addFileTurn(s *Session, turn FileTurn, limit int) error {
	return sm.updateFileTurns(s, func(turns *[]FileTurn) error {
		pending := 0
		for _, existing := range *turns {
			if existing.Principal.MessageID == turn.Principal.MessageID {
				return nil
			}
			if unfinishedFileTurn(existing) {
				pending++
			}
		}
		if pending >= limit || len(turn.Content) > 128<<10 {
			return errFileTurnStorage
		}
		// ponytail: retain 128 terminal receipts; platform dedup covers older
		// transport redeliveries. Never evict an unfinished supplement.
		for len(*turns) >= retainedFileTurns {
			index := -1
			for i, old := range *turns {
				if !unfinishedFileTurn(old) {
					index = i
					break
				}
			}
			if index < 0 {
				return errFileTurnStorage
			}
			*turns = append((*turns)[:index], (*turns)[index+1:]...)
		}
		*turns = append(*turns, turn)
		return nil
	})
}

func (sm *SessionManager) setFileTurnStatus(s *Session, messageID, status string) error {
	matched := false
	for _, turn := range s.fileTurns() {
		matched = matched || turn.Principal.MessageID == messageID || turn.ResumeMessageID == messageID
	}
	if !matched {
		return nil
	} // Ordinary/initial turns keep their existing path.
	return sm.updateFileTurns(s, func(turns *[]FileTurn) error {
		for i := range *turns {
			t := &(*turns)[i]
			if (t.Principal.MessageID == messageID || t.ResumeMessageID == messageID) && unfinishedFileTurn(*t) {
				t.Status = status
			}
		}
		return nil
	})
}
