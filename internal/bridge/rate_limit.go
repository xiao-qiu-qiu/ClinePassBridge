package bridge

import (
	"fmt"
	"sync"
	"time"
)

type rateKey struct {
	credential  string
	model       string
	fingerprint [32]byte
}

type rateWindow struct {
	id      string
	retryAt time.Time
	scope   string
	probe   *rateLease
	epoch   uint64
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[rateKey]*rateWindow
	clock   func() time.Time
}

type rateLease struct {
	owner *rateLimiter
	key   rateKey
	state *rateWindow
	epoch uint64
}

func (l *rateLimiter) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

func (l *rateLimiter) acquire(c Credential, model string) (*rateLease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.windows == nil {
		l.windows = make(map[rateKey]*rateWindow)
	}
	// Expired idle windows have no persistent quota meaning. Bound retained
	// keys after credential rotation/model edits without disturbing live probes.
	for key, state := range l.windows {
		if state.probe == nil && now.Sub(state.retryAt) > 10*time.Minute {
			delete(l.windows, key)
		}
	}
	key := rateKey{c.ID, model, usageFingerprint(c)}
	lease := &rateLease{owner: l, key: key}
	state := l.windows[key]
	if state == nil {
		return lease, nil
	}
	if now.Before(state.retryAt) || state.probe != nil {
		until := state.retryAt
		reason := "upstream rate limit window"
		if state.probe != nil && !now.Before(until) {
			until, reason = now.Add(time.Second), "upstream recovery probe in progress"
		}
		// A locally suppressed request never reached the upstream, so it has no
		// provider of its own and must not borrow the one from the 429 that
		// opened the window. The UI labels these rows from UpstreamSkipped.
		return nil, &upstreamError{APIError: &APIError{429, "upstream_team_rate_limited", fmt.Sprintf("%s Upstream shared team/region token rate limit. Retry after %ds. %s; no upstream request sent", rateLimitMarker, retrySeconds(until, now), reason)}, RetryAt: until, Scope: state.scope, Local: true, WindowID: state.id}
	}
	state.probe = lease
	lease.state, lease.epoch = state, state.epoch
	return lease, nil
}

func (lease *rateLease) finish(err error) {
	if lease == nil {
		return
	}
	l := lease.owner
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.windows[lease.key]
	if limited := asTeamRateLimit(err); limited != nil && !limited.Local {
		if state == nil {
			state = &rateWindow{id: id()}
			l.windows[lease.key] = state
		}
		if limited.RetryAt.After(state.retryAt) {
			state.retryAt = limited.RetryAt
		}
		state.scope = limited.Scope
		state.epoch++
		if state.probe == lease {
			state.probe = nil
		}
		return
	}
	// A success from an older in-flight call must not erase a newer limit.
	if state != nil && state == lease.state && state.probe == lease {
		state.probe = nil
		if state.epoch == lease.epoch && err == nil {
			delete(l.windows, lease.key)
		} else if err != nil {
			// Cancellation/transport failure is not evidence of recovery. Keep
			// single-probe admission instead of reopening all callers at once.
			if retryAt := l.now().Add(5 * time.Second); retryAt.After(state.retryAt) {
				state.retryAt = retryAt
			}
		}
	}
}
