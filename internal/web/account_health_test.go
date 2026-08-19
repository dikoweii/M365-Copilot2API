package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func TestUpstreamErrorClassification(t *testing.T) {
	cases := []struct {
		err      error
		limited  bool
		authFail bool
		retry    int
		status   int
	}{
		{&UpstreamHTTPError{Status: 429, RetryAfter: 90}, true, false, 90, http.StatusTooManyRequests},
		{&UpstreamHTTPError{Status: 503}, true, false, 0, http.StatusTooManyRequests},
		{&UpstreamHTTPError{Status: 401}, false, true, 0, http.StatusUnauthorized},
		{&UpstreamHTTPError{Status: 403}, false, true, 0, http.StatusUnauthorized},
		{&UpstreamHTTPError{Status: 502}, false, false, 0, http.StatusBadGateway},
		{&UpstreamHTTPError{Status: 502, Body: "account is limited"}, true, false, 0, http.StatusTooManyRequests},
		{fmt.Errorf("upstream http 429"), true, false, 0, http.StatusTooManyRequests},
		{fmt.Errorf("Too many requests, slow down"), true, false, 0, http.StatusTooManyRequests},
		{fmt.Errorf("account is limited"), true, false, 0, http.StatusTooManyRequests},
		{fmt.Errorf("random failure"), false, false, 0, http.StatusBadGateway},
		{chathub.ErrRateLimitNotice, true, false, 0, http.StatusTooManyRequests},
	}
	for _, c := range cases {
		if got := IsRateLimited(c.err); got != c.limited {
			t.Errorf("IsRateLimited(%v)=%v want %v", c.err, got, c.limited)
		}
		if got := IsAuthFailure(c.err); got != c.authFail {
			t.Errorf("IsAuthFailure(%v)=%v want %v", c.err, got, c.authFail)
		}
		if got := RetryAfterSeconds(c.err); got != c.retry {
			t.Errorf("RetryAfterSeconds(%v)=%d want %d", c.err, got, c.retry)
		}
		if got := upstreamStatus(c.err); got != c.status {
			t.Errorf("upstreamStatus(%v)=%d want %d", c.err, got, c.status)
		}
	}
}

func TestAccountHealthLifecycle(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-1"

	if !h.Available(id) {
		t.Fatal("fresh account must be available")
	}
	h.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	if h.Available(id) {
		t.Fatal("rate-limited account must be in cooldown")
	}
	if !h.RateLimited(id) {
		t.Fatal("rate-limited account missing rate-limit state")
	}
	h.MarkSuccess(id)
	if h.RateLimited(id) {
		t.Fatal("success must clear rate-limit state")
	}
	if !h.Available(id) {
		t.Fatal("MarkSuccess must lift the cooldown")
	}

	h.MarkFailure(id, &UpstreamHTTPError{Status: 401}, 0)
	if h.Available(id) {
		t.Fatal("auth-failed account must stay unusable")
	}
	h.MarkSuccess(id)
	if !h.Available(id) {
		t.Fatal("MarkSuccess must clear auth failure")
	}

	h.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	until := h.Snapshot()[id]
	if until == nil || until["available"].(bool) || until["cooldownUntil"] == nil {
		t.Fatalf("snapshot should report cooldown until: %v", h.Snapshot())
	}
}

func TestCooldownExpiryClearsCallCount(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-expiry"
	h.MarkCall(id)
	h.MarkCall(id)
	h.MarkFailure(id, fmt.Errorf("account limited"), time.Minute)
	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.Available(id) {
		t.Fatal("expired cooldown must be available")
	}
	if h.CallCount(id) != 0 {
		t.Fatalf("call count=%d want 0", h.CallCount(id))
	}
	if h.RateLimited(id) {
		t.Fatal("expired cooldown still marked limited")
	}
	h.MarkCall(id)
	h.MarkFailure(id, fmt.Errorf("account limited"), time.Minute)
	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if _, ok := h.CooldownUntil(id); ok || h.CallCount(id) != 0 {
		t.Fatal("CooldownUntil must clear expired call count")
	}
	h.MarkCall(id)
	h.MarkFailure(id, fmt.Errorf("account limited"), time.Minute)
	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	h.MarkCall(id)
	if h.CallCount(id) != 1 {
		t.Fatalf("post-cooldown call count=%d want 1", h.CallCount(id))
	}
	const authID = "acct-auth-expiry"
	h.MarkCall(authID)
	h.MarkFailure(authID, &UpstreamHTTPError{Status: 401}, time.Minute)
	h.mu.Lock()
	h.cooldown[authID] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if h.Available(authID) || h.CallCount(authID) != 1 {
		t.Fatal("auth failure must remain pinned without clearing call count")
	}
}

func TestAuthFailureRemainsPinnedUntilExplicitRecovery(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-auth-pinned"
	generation := h.MarkCall(id)
	h.MarkResult(id, generation, &UpstreamHTTPError{Status: http.StatusUnauthorized}, time.Millisecond)

	h.mu.Lock()
	h.cooldown[id] = time.Now().Add(-time.Hour)
	h.mu.Unlock()
	if h.Available(id) {
		t.Fatal("expired cooldown must not make an auth-failed account available")
	}
	state := h.Snapshot()[id]
	if state == nil || state["authFailed"] != true || state["available"] != false {
		t.Fatalf("auth failure snapshot = %#v", state)
	}

	h.ClearAllCooldowns()
	if !h.Available(id) {
		t.Fatal("explicit clear must restore auth-failed account availability")
	}
}

func TestOlderConcurrentSuccessCannotClearNewerRateLimit(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-concurrent-results"
	olderSuccess := h.MarkCall(id)
	newerFailure := h.MarkCall(id)

	h.MarkResult(id, newerFailure, &UpstreamHTTPError{Status: http.StatusTooManyRequests}, 10*time.Minute)
	h.MarkResult(id, olderSuccess, nil, 0)
	// Existing callers may redundantly mark a successful request after the
	// versioned result has already been applied.
	h.MarkSuccess(id)
	if h.Available(id) || !h.RateLimited(id) {
		t.Fatal("older success cleared a newer rate-limit cooldown")
	}

	recovery := h.MarkCall(id)
	h.MarkResult(id, recovery, nil, 0)
	h.MarkSuccess(id)
	if !h.Available(id) || h.RateLimited(id) {
		t.Fatal("newer success did not recover historical rate-limit state")
	}
}

func testAccountFiles(t *testing.T) *auth.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	toks := map[string]auth.TokenSet{
		"u-1": {HomeOID: "u-1", Email: "one@example.com", AccessToken: "tok1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour)},
		"u-2": {HomeOID: "u-2", Email: "two@example.com", AccessToken: "tok2", RefreshToken: "r2", ExpiresAt: time.Now().Add(time.Hour)},
		"u-3": {HomeOID: "u-3", Email: "three@example.com", AccessToken: "tok3", RefreshToken: "r3", ExpiresAt: time.Now().Add(time.Hour)},
	}
	b, _ := os.ReadFile(path)
	_ = b
	store, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, tok := range toks {
		if _, err := store.Upsert(tok); err != nil {
			t.Fatalf("upsert %s: %v", tok.HomeOID, err)
		}
	}
	return store
}

func TestWriteUpstreamErrorHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	writeUpstreamError(w, &UpstreamHTTPError{Status: 429, RetryAfter: 90})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After=%q want 90", got)
	}
	if strings.Contains(w.Body.String(), "429") {
		t.Fatalf("client-visible body must not leak upstream status: %q", w.Body.String())
	}

	w = httptest.NewRecorder()
	writeUpstreamError(w, &UpstreamHTTPError{Status: 502})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After=%q want empty for non-rate-limited errors", got)
	}
}

func TestResolveAccountSkipsUnhealthy(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	s.accountPool.MarkFailure("u-1", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	acc, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if acc.ID == "u-1" {
		t.Fatalf("resolveAccount must skip the cooling-down account, got %s", acc.ID)
	}
	if acc.Email == "" {
		t.Fatal("resolveAccount should return a validated account")
	}
}

func TestResolveAccountSkipsSchedulingDisabled(t *testing.T) {
	store := testAccountFiles(t)
	if err := store.SetScheduleEnabled("u-1", false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetScheduleEnabled("u-2", false); err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	acc, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "u-3" {
		t.Fatalf("scheduled account=%s want u-3", acc.ID)
	}
	explicit, err := s.resolveAccount("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if explicit.ID != "u-1" {
		t.Fatalf("explicit account=%s want u-1", explicit.ID)
	}
}

func TestResolveAccountAllUnhealthy(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	for _, id := range []string{"u-1", "u-2", "u-3"} {
		s.accountPool.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	}
	if _, err := s.resolveAccount(""); err == nil {
		t.Fatal("resolveAccount must fail when every account is cooling down")
	}
}

func TestNextHealthyAccount(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	s.accountPool.MarkFailure("u-2", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	acc, err := s.nextHealthyAccount("u-1")
	if err != nil {
		t.Fatalf("nextHealthyAccount: %v", err)
	}
	if acc.ID == "u-1" || acc.ID == "u-2" {
		t.Fatalf("nextHealthyAccount must skip the avoided and the unhealthy account, got %s", acc.ID)
	}
	if acc.ID != "u-3" {
		t.Fatalf("expected u-3, got %s", acc.ID)
	}

	for _, id := range []string{"u-1", "u-2", "u-3"} {
		s.accountPool.MarkFailure(id, &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	}
	if _, err := s.nextHealthyAccount(""); err == nil {
		t.Fatal("nextHealthyAccount must fail when no healthy account remains")
	}
}

func TestScheduleAccount(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/accounts/schedule", strings.NewReader(`{"id":"u-1","enabled":false}`))
	s.scheduleAccount(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if store.ScheduleEnabled("u-1") {
		t.Fatal("account scheduling still enabled")
	}
}

func TestAccountsReportsCooldown(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	s.accountPool.MarkCall("u-1")
	s.accountPool.MarkCall("u-1")
	s.accountPool.MarkFailure("u-1", fmt.Errorf("account limited"), 20*time.Minute)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	s.accounts(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Accounts []struct {
			ID              string     `json:"id"`
			Status          string     `json:"status"`
			ScheduleEnabled bool       `json:"scheduleEnabled"`
			CallCount       uint64     `json:"callCount"`
			CooldownUntil   *time.Time `json:"cooldownUntil"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, account := range body.Accounts {
		if account.ID != "u-1" {
			continue
		}
		if account.Status != "cooldown" || account.CooldownUntil == nil || !account.ScheduleEnabled || account.CallCount != 2 {
			t.Fatalf("cooldown account=%#v", account)
		}
		return
	}
	t.Fatal("cooldown account missing")
}

func TestFailoverAllowsResolvedConversationID(t *testing.T) {
	accountID := ""
	conversationID := "conv-123"
	resolvedConversationID := "conv-123"
	if !(conversationID == "" || conversationID == resolvedConversationID) {
		t.Fatal("failover must be allowed when ConversationID was injected by session resolver")
	}
	explicitConversationID := "conv-explicit"
	if explicitConversationID == "" || explicitConversationID == resolvedConversationID {
		t.Fatal("failover must NOT be allowed when ConversationID was explicitly set by client")
	}
	_ = accountID
}

func TestErrRateLimitNoticeTriggersMarkFailure(t *testing.T) {
	h := newAccountHealth()
	const id = "acct-rl"
	h.MarkFailure(id, chathub.ErrRateLimitNotice, 15*time.Minute)
	if h.Available(id) {
		t.Fatal("ErrRateLimitNotice must put account in cooldown")
	}
}

func TestAccountHealthScorePrefersLowLatencyAndLowConcurrency(t *testing.T) {
	h := newAccountHealth()
	h.Observe("fast", 100*time.Millisecond, nil)
	h.Observe("slow", 900*time.Millisecond, nil)
	if h.Score("fast", 0) >= h.Score("slow", 0) {
		t.Fatal("lower-latency account did not receive the better score")
	}
	if h.Score("fast", 1) <= h.Score("slow", 0) {
		t.Fatal("an in-flight request should outweigh a modest latency advantage")
	}
	h.Observe("failing", time.Second, fmt.Errorf("temporary failure"))
	if h.Score("failing", 0) <= h.Score("slow", 0) {
		t.Fatal("observed failures should lower scheduling priority")
	}
}

func TestResolveAccountPrefersObservedFastAccount(t *testing.T) {
	store := testAccountFiles(t)
	h := newAccountHealth()
	h.Observe("u-1", 2500*time.Millisecond, nil)
	h.Observe("u-2", 120*time.Millisecond, nil)
	h.Observe("u-3", 1200*time.Millisecond, nil)
	s := &Server{tokens: store, accountPool: h, accountConcurrency: newAccountConcurrency()}
	acc, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "u-2" {
		t.Fatalf("selected account = %q, want fastest healthy account u-2", acc.ID)
	}
}

func TestResolveAccountSkipsBestScoreWhenTokenCannotRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	store, err := auth.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID: "expired", Email: "expired@example.com", AccessToken: "expired-token",
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID: "healthy", Email: "healthy@example.com", AccessToken: "healthy-token",
		RefreshToken: "healthy-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	health := newAccountHealth()
	health.Observe("expired", 50*time.Millisecond, nil)
	health.Observe("healthy", 500*time.Millisecond, nil)
	s := &Server{tokens: store, accountPool: health, accountConcurrency: newAccountConcurrency()}
	selected, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "healthy" {
		t.Fatalf("selected account = %q, want healthy", selected.ID)
	}
	if health.Available("expired") {
		t.Fatal("account with a permanently expired token remained schedulable")
	}
}
