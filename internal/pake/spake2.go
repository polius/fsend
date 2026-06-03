package pake

import (
	"errors"
	"fmt"
	"sync"

	gospake2 "salsa.debian.org/vasudev/gospake2"
)

// spake2Impl is the gospake2-backed implementation of PAKE.
type spake2Impl struct {
	mu       sync.Mutex
	state    *gospake2.SPAKE2
	started  bool
	finished bool
}

func newSpake2Impl(code, sessionID string) PAKE {
	// Symmetric SPAKE2: both peers play the same role.
	// The shared identity string binds the session: same code + different
	// sessionID derives an entirely different key. This is the
	// channel-binding flavor specified in docs/decisions/pake.md.
	identity := gospake2.NewIdentityS("fsend|" + sessionID)
	password := gospake2.NewPassword(code)
	state := gospake2.SPAKE2Symmetric(password, identity)
	return &spake2Impl{state: &state}
}

func (s *spake2Impl) Start() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil, errors.New("pake: Start called twice")
	}
	s.started = true
	return s.state.Start(), nil
}

func (s *spake2Impl) Finish(peerMessage []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return nil, errors.New("pake: Finish before Start")
	}
	if s.finished {
		return nil, errors.New("pake: Finish called twice")
	}
	s.finished = true
	key, err := s.state.Finish(peerMessage)
	if err != nil {
		return nil, fmt.Errorf("pake: SPAKE2 finish failed: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("pake: derived key length %d, expected %d", len(key), KeySize)
	}
	return key, nil
}
