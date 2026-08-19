package web

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/chathub"
)

// UpstreamHTTPError carries the HTTP status of a failed upstream request so
// callers can distinguish rate limiting (429), authorization issues (401/403)
// and transient server errors (5xx) from one another.
type UpstreamHTTPError struct {
	Status     int
	RetryAfter int
	Body       string
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream http %d", e.Status)
}

// IsRateLimited reports whether err represents an upstream 429 or an
// indistinguishable throttling signal (rate limit, too many requests,
// throttled).
func IsRateLimited(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, chathub.ErrRateLimitNotice) {
		return true
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.Status == 429 || httpErr.Status == 503 {
			return true
		}
		if strings.Contains(strings.ToLower(httpErr.Body), "limited") {
			return true
		}
	}
	var dialErr *chathub.DialError
	if errors.As(err, &dialErr) {
		return dialErr.Status == 429 || dialErr.Status == 503
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "limited") ||
		strings.Contains(msg, "throttl")
}

// IsAuthFailure reports whether err represents an upstream 401/403, meaning
// the account itself is unusable until re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == 401 || httpErr.Status == 403
	}
	var dialErr *chathub.DialError
	if errors.As(err, &dialErr) {
		return dialErr.Status == 401 || dialErr.Status == 403
	}
	return false
}

func IsEmptyCompletion(err error) bool {
	return errors.Is(err, chathub.ErrEmptyCompletion)
}

// RetryAfterSeconds returns the upstream Retry-After hint for a rate-limited
// error, or 0 when absent. The web layer surfaces this to clients so they can
// back off instead of hammering a throttled pool.
func RetryAfterSeconds(err error) int {
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.RetryAfter
	}
	var dialErr *chathub.DialError
	if errors.As(err, &dialErr) {
		return dialErr.RetryAfter
	}
	return 0
}

// accountHealth tracks per-account failure state: rate-limited accounts are
// cooled down and skipped by the round-robin until the window expires, and
// auth-failed accounts are pinned as unusable.
type accountHealth struct {
	mu                sync.Mutex
	cooldown          map[string]time.Time
	authFail          map[string]bool
	limited           map[string]bool
	calls             map[string]uint64
	latency           map[string]float64
	success           map[string]uint64
	failures          map[string]uint64
	generation        map[string]uint64
	stateGeneration   map[string]uint64
	successGeneration map[string]uint64
}

func newAccountHealth() *accountHealth {
	return &accountHealth{
		cooldown: map[string]time.Time{}, authFail: map[string]bool{}, limited: map[string]bool{}, calls: map[string]uint64{},
		latency: map[string]float64{}, success: map[string]uint64{}, failures: map[string]uint64{},
		generation: map[string]uint64{}, stateGeneration: map[string]uint64{}, successGeneration: map[string]uint64{},
	}
}

// Observe records an exponentially weighted response latency and a rolling
// success/failure ratio used by account selection. Health cooldown remains the
// hard eligibility boundary; this score only orders otherwise healthy peers.
func (h *accountHealth) Observe(accountID string, latency time.Duration, err error) {
	if h == nil || accountID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.failures[accountID]++
		return
	}
	h.success[accountID]++
	ms := float64(latency.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	if previous := h.latency[accountID]; previous > 0 {
		h.latency[accountID] = previous*0.8 + ms*0.2
	} else {
		h.latency[accountID] = ms
	}
}

func (h *accountHealth) Score(accountID string, inflight int) float64 {
	if h == nil {
		return float64(inflight) * 1000
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	latency := h.latency[accountID]
	if latency == 0 {
		latency = 1500
	}
	total := h.success[accountID] + h.failures[accountID]
	failureRate := 0.0
	if total > 0 {
		failureRate = float64(h.failures[accountID]) / float64(total)
	}
	return float64(inflight)*2000 + latency + failureRate*5000 + float64(h.calls[accountID])*0.01
}

func (h *accountHealth) cleanupExpiredCooldownLocked(accountID string) {
	until, ok := h.cooldown[accountID]
	if !ok || time.Now().Before(until) {
		return
	}
	rateLimited := h.limited[accountID]
	delete(h.cooldown, accountID)
	delete(h.limited, accountID)
	if rateLimited {
		delete(h.calls, accountID)
	}
}

func (h *accountHealth) MarkCall(accountID string) uint64 {
	if h == nil || accountID == "" {
		return 0
	}
	h.mu.Lock()
	h.cleanupExpiredCooldownLocked(accountID)
	h.calls[accountID]++
	h.generation[accountID]++
	generation := h.generation[accountID]
	h.mu.Unlock()
	return generation
}

func (h *accountHealth) CallCount(accountID string) uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	return h.calls[accountID]
}

func (h *accountHealth) RateLimited(accountID string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	return h.limited[accountID]
}

func (h *accountHealth) MarkFailure(accountID string, err error, window time.Duration) {
	if h == nil || accountID == "" || (!IsAuthFailure(err) && !IsRateLimited(err)) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	if !h.authFail[accountID] && !h.limited[accountID] {
		if h.generation[accountID] > 0 {
			h.generation[accountID]++
			h.stateGeneration[accountID] = h.generation[accountID]
		}
	}
	h.markFailureLocked(accountID, err, window)
}

// MarkResult applies a request result only when its start generation is not
// older than the health state already observed for the account.
func (h *accountHealth) MarkResult(accountID string, generation uint64, err error, window time.Duration) {
	if h == nil || accountID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation > h.generation[accountID] {
		h.generation[accountID] = generation
	}
	if err == nil {
		if generation > h.successGeneration[accountID] {
			h.successGeneration[accountID] = generation
		}
		if generation < h.stateGeneration[accountID] {
			return
		}
		h.stateGeneration[accountID] = generation
		h.clearFailureLocked(accountID)
		return
	}
	if !IsAuthFailure(err) && !IsRateLimited(err) {
		return
	}
	if generation < h.stateGeneration[accountID] {
		return
	}
	h.stateGeneration[accountID] = generation
	h.markFailureLocked(accountID, err, window)
}

// MarkSuccess clears any failure state after a healthy response.
func (h *accountHealth) MarkSuccess(accountID string) {
	if h == nil || accountID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	if h.stateGeneration[accountID] > h.successGeneration[accountID] && (h.authFail[accountID] || h.limited[accountID]) {
		return
	}
	h.clearFailureLocked(accountID)
}

func (h *accountHealth) markFailureLocked(accountID string, err error, window time.Duration) {
	if IsAuthFailure(err) {
		h.authFail[accountID] = true
		delete(h.cooldown, accountID)
		delete(h.limited, accountID)
		return
	}
	if window <= 0 {
		window = 60 * time.Second
	}
	delete(h.authFail, accountID)
	h.limited[accountID] = true
	if ra := RetryAfterSeconds(err); ra > 0 {
		window = time.Duration(ra) * time.Second
		if window > 30*time.Minute {
			window = 30 * time.Minute
		}
	}
	h.cooldown[accountID] = time.Now().Add(window)
}

func (h *accountHealth) clearFailureLocked(accountID string) {
	delete(h.cooldown, accountID)
	delete(h.authFail, accountID)
	delete(h.limited, accountID)
}

// Available reports whether the account may be used right now.
func (h *accountHealth) Available(accountID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	if h.authFail[accountID] {
		return false
	}
	if until, ok := h.cooldown[accountID]; ok && time.Now().Before(until) {
		return false
	}
	return true
}

func (h *accountHealth) CooldownUntil(accountID string) (time.Time, bool) {
	if h == nil {
		return time.Time{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredCooldownLocked(accountID)
	until, ok := h.cooldown[accountID]
	if !ok {
		return time.Time{}, false
	}
	return until, true
}

// Snapshot returns a copy of the current health state for the admin UI.
func (h *accountHealth) Snapshot() map[string]map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]map[string]any, len(h.cooldown)+len(h.authFail)+len(h.latency))
	for id, until := range h.cooldown {
		out[id] = map[string]any{"available": time.Now().After(until), "cooldownUntil": until}
	}
	for id, failed := range h.authFail {
		if failed {
			if _, ok := out[id]; !ok {
				out[id] = map[string]any{}
			}
			out[id]["available"] = false
			out[id]["authFailed"] = true
		}
	}
	for id, latency := range h.latency {
		if _, ok := out[id]; !ok {
			out[id] = map[string]any{}
		}
		out[id]["ewmaLatencyMs"] = int64(latency)
		out[id]["successes"] = h.success[id]
		out[id]["failures"] = h.failures[id]
	}
	return out
}

func (h *accountHealth) ClearAllCooldowns() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cooldown = map[string]time.Time{}
	h.authFail = map[string]bool{}
	h.limited = map[string]bool{}
	h.calls = map[string]uint64{}
	h.stateGeneration = map[string]uint64{}
	h.successGeneration = map[string]uint64{}
}

// EarliestRecovery returns the earliest time at which any account may become
// available again. Used to populate Retry-After when all accounts are cooling.
func (h *accountHealth) EarliestRecovery() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	earliest := time.Now().Add(5 * time.Minute)
	for _, until := range h.cooldown {
		if until.Before(earliest) {
			earliest = until
		}
	}
	return earliest
}
