package web

import (
	"strings"
	"sync"
	"time"
)

// accountHealth tracks per-account upstream failures so the gateway can skip
// accounts that are currently rate-limited or auth-broken instead of hammering
// them. It backs the per-account 429 failover (D 项).
type accountHealth struct {
	mu       sync.Mutex
	cooldown map[string]time.Time // rate-limit cooldown deadline per account
	authFail map[string]bool      // hard pin: account auth broken, skip until re-login
}

func newAccountHealth() *accountHealth {
	return &accountHealth{
		cooldown: make(map[string]time.Time),
		authFail: make(map[string]bool),
	}
}

// rateLimitCooldown is how long a rate-limited account is skipped before it may
// be selected again.
const rateLimitCooldown = 2 * time.Minute

// MarkRateLimited puts the account on cooldown until the given time. A zero
// time defaults to now+rateLimitCooldown.
func (h *accountHealth) MarkRateLimited(id string, until time.Time) {
	if until.IsZero() {
		until = time.Now().Add(rateLimitCooldown)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cooldown[id] = until
	delete(h.authFail, id)
}

// MarkAuthFail pins the account as auth-broken (e.g. 401/403). It is skipped
// until re-login clears it via MarkSuccess.
func (h *accountHealth) MarkAuthFail(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authFail[id] = true
	delete(h.cooldown, id)
}

// MarkSuccess clears any failure state for the account.
func (h *accountHealth) MarkSuccess(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.cooldown, id)
	delete(h.authFail, id)
}

// Available reports whether the account can be selected for a new request.
func (h *accountHealth) Available(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.authFail[id] {
		return false
	}
	if t, ok := h.cooldown[id]; ok && time.Now().Before(t) {
		return false
	}
	return true
}

// accountHealthView is the per-account health snapshot for the admin UI.
type accountHealthView struct {
	AccountID     string `json:"accountId"`
	RateLimited   bool   `json:"rateLimited"`
	AuthFail      bool   `json:"authFail"`
	CooldownUntil string `json:"cooldownUntil,omitempty"`
}

// Snapshot returns a copy of the current health state.
func (h *accountHealth) Snapshot() []accountHealthView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]accountHealthView, 0, len(h.cooldown)+len(h.authFail))
	now := time.Now()
	for id, t := range h.cooldown {
		out = append(out, accountHealthView{
			AccountID:     id,
			RateLimited:   now.Before(t),
			CooldownUntil: t.Format(time.RFC3339),
		})
	}
	for id := range h.authFail {
		out = append(out, accountHealthView{AccountID: id, AuthFail: true})
	}
	return out
}

// IsRateLimited reports whether err represents an upstream rate-limit (429/503).
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"429", "too many requests", "rate limit", "ratelimit", "throttl", "quota", "frequency"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// IsAuthFailure reports whether err represents an auth failure (401/403).
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"401", "403", "unauthorized", "forbidden", "invalid_grant", "token expired", "authentication failed"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
