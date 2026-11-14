package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/polius/fsend/internal/atomicfile"
	"github.com/polius/fsend/internal/code"
)

// Restart survival: on graceful shutdown the server can save its pending
// (unpaired) sessions and reload them on the next start, so a redeploy
// doesn't kill every code a sender is still waiting on. Paired sessions are
// not saved: their relay allocations die with the process anyway.
//
// The snapshot holds no IP addresses: a restored session learns the
// sender's address again from its next /wait poll. It does hold the slot and
// the session's random credentials, so it is written 0600 and deleted as
// soon as it is loaded.

const stateVersion = 1

type stateFile struct {
	Version  int            `json:"version"`
	Sessions []savedSession `json:"sessions"`
	// RelayBudget is the relay's spend for the day, so a redeploy doesn't
	// reset the daily byte cap. Absent when nothing was forwarded.
	RelayBudget *savedBudget `json:"relay_budget,omitempty"`
}

type savedBudget struct {
	Day  int64  `json:"day"` // the UTC day's midnight, unix seconds
	Used uint64 `json:"used"`
}

// budgetKeeper is the part of the relay that can carry its daily byte
// count across a restart (relay.Server).
type budgetKeeper interface {
	BudgetUsage(now time.Time) (day int64, used uint64)
	RestoreBudget(now time.Time, day int64, used uint64) bool
}

type savedSession struct {
	ID          string    `json:"id"`
	Slot        string    `json:"slot"`
	SenderToken string    `json:"sender_token"`
	SenderICE   IceCreds  `json:"sender_ice"`
	CreatedAt   time.Time `json:"created_at"`
}

// ReleaseWaiters answers every parked and future /wait long-poll with 204 at
// once, so a shutdown doesn't hold them for the full poll timeout. The
// sender just re-polls, reaching the next process.
func (s *Server) ReleaseWaiters() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

// SaveState writes the pending sessions to path and reports how many. Call
// it after the HTTP server has stopped, so nothing changes underneath.
func (s *Server) SaveState(path string) (int, error) {
	st := stateFile{Version: stateVersion}
	s.mu.Lock()
	for _, sess := range s.byID {
		if sess.State != "waiting" {
			continue
		}
		st.Sessions = append(st.Sessions, savedSession{
			ID:          sess.ID,
			Slot:        sess.Slot,
			SenderToken: sess.SenderToken,
			SenderICE:   sess.SenderICE,
			CreatedAt:   sess.CreatedAt,
		})
	}
	s.mu.Unlock()
	if k, ok := s.relayAllocator.(budgetKeeper); ok {
		if day, used := k.BudgetUsage(time.Now()); used > 0 {
			st.RelayBudget = &savedBudget{Day: day, Used: used}
		}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return 0, err
	}
	return len(st.Sessions), atomicfile.Write(path, data, 0o600)
}

// LoadState restores sessions saved by SaveState and deletes the file. A
// missing file is not an error. Sessions past UnpairedTTL, malformed, or
// clashing with a live one are dropped.
func (s *Server) LoadState(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// Read once: a stale snapshot must never resurrect sessions later.
	if err := os.Remove(path); err != nil {
		return 0, err
	}
	var st stateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return 0, fmt.Errorf("server state %s: %w", path, err)
	}
	if st.Version != stateVersion {
		return 0, fmt.Errorf("server state %s: unsupported version %d", path, st.Version)
	}
	now := time.Now()
	if k, ok := s.relayAllocator.(budgetKeeper); ok && st.RelayBudget != nil &&
		k.RestoreBudget(now, st.RelayBudget.Day, st.RelayBudget.Used) {
		s.cfg.Logger.Info("restored relay daily byte count", "bytes", st.RelayBudget.Used)
	}
	n := 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range st.Sessions {
		if now.Sub(ss.CreatedAt) > s.cfg.UnpairedTTL || code.ValidateSlot(ss.Slot) != nil ||
			ss.ID == "" || ss.SenderToken == "" {
			continue
		}
		if _, taken := s.bySlot[ss.Slot]; taken {
			continue
		}
		if _, taken := s.byID[ss.ID]; taken {
			continue
		}
		sess := &session{
			ID:          ss.ID,
			Slot:        ss.Slot,
			SenderToken: ss.SenderToken,
			SenderICE:   ss.SenderICE,
			State:       "waiting",
			CreatedAt:   ss.CreatedAt,
			LastSeen:    now, // the sender re-polls within its retry backoff
			waiters:     make(chan struct{}),
		}
		s.bySlot[ss.Slot] = sess
		s.byID[ss.ID] = sess
		n++
	}
	return n, nil
}
