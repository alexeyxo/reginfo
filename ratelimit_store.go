package reginfo

import (
	"context"
	"errors"
	"sync"
	"time"
)

type RateLimitStore interface {
	Acquire(ctx context.Context, key string, limits []RateLimit) (time.Duration, error)
	Block(ctx context.Context, key string, until time.Time) (time.Time, error)
}

type MemoryRateLimitStore struct {
	mu     sync.Mutex
	states map[string]*serverLimit
}

type serverLimit struct {
	windows []rateWindow
	blocked time.Time
}

type rateWindow struct {
	limit RateLimit
	sent  []time.Time
}

var _ RateLimitStore = (*MemoryRateLimitStore)(nil)

func NewMemoryRateLimitStore() *MemoryRateLimitStore {
	return &MemoryRateLimitStore{}
}

func (s *MemoryRateLimitStore) Acquire(ctx context.Context, key string, limits []RateLimit) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(limits) == 0 {
		return 0, errors.New("rate limits must not be empty")
	}
	if err := validateRateLimits(limits); err != nil {
		return 0, err
	}
	state := s.state(key)
	if len(state.windows) == 0 {
		for _, limit := range limits {
			state.windows = append(state.windows, rateWindow{limit: limit})
		}
	} else if !state.matches(limits) {
		return 0, errors.New("inconsistent rate limits for shared store key")
	}
	now := time.Now()
	ready := state.blocked
	for i := range state.windows {
		window := &state.windows[i]
		cutoff := now.Add(-window.limit.Interval)
		for len(window.sent) > 0 && !window.sent[0].After(cutoff) {
			window.sent = window.sent[1:]
		}
		if len(window.sent) >= window.limit.Requests {
			next := window.sent[0].Add(window.limit.Interval)
			if next.After(ready) {
				ready = next
			}
		}
	}
	if ready.After(now) {
		return ready.Sub(now), nil
	}
	for i := range state.windows {
		state.windows[i].sent = append(state.windows[i].sent, now)
	}
	return 0, nil
}

func (s *MemoryRateLimitStore) Block(ctx context.Context, key string, until time.Time) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	state := s.state(key)
	if until.After(state.blocked) {
		state.blocked = until
	}
	return state.blocked, nil
}

func (s *MemoryRateLimitStore) state(key string) *serverLimit {
	if s.states == nil {
		s.states = make(map[string]*serverLimit)
	}
	if state := s.states[key]; state != nil {
		return state
	}
	state := &serverLimit{}
	s.states[key] = state
	return state
}

func (s *serverLimit) matches(limits []RateLimit) bool {
	if len(s.windows) != len(limits) {
		return false
	}
	for i, window := range s.windows {
		if window.limit != limits[i] {
			return false
		}
	}
	return true
}
