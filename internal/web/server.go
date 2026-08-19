package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/mcp"
	"m365-copilot2api/internal/outbound"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type pendingPKCE struct {
	Verifier    string
	Created     time.Time
	Status      string
	Account     any
	Error       string
	RedirectURI string
}

const rateLimitCooldown = 30 * time.Second

const maxAccountProbe = 16

const rateLimitProbePrompt = "Reply with exactly: OK"

func (s *Server) markAccountResult(accountID string, err error) {
	if s == nil || s.accountPool == nil || accountID == "" {
		return
	}
	if err != nil {
		s.accountPool.MarkFailure(accountID, err, rateLimitCooldown)
		return
	}
	s.accountPool.MarkSuccess(accountID)
}

// confirmRateLimitNotice verifies a text-channel rate-limit notice with a
// separate, fresh ChatHub conversation. A single notice is not enough to cool
// down an account because the upstream can occasionally emit a false positive.
func (s *Server) confirmRateLimitNotice(ctx context.Context, acc auth.AccountToken, noticeErr error) (bool, error) {
	if !errors.Is(noticeErr, chathub.ErrRateLimitNotice) {
		return IsRateLimited(noticeErr), noticeErr
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, probeErr := s.chatWithAccount(probeCtx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:    rateLimitProbePrompt,
		Tone:    "magic",
		Started: true,
	})
	if probeErr == nil {
		return false, nil
	}
	if errors.Is(probeErr, chathub.ErrRateLimitNotice) || IsRateLimited(probeErr) {
		return true, &UpstreamHTTPError{
			Status:     http.StatusTooManyRequests,
			RetryAfter: int(rateLimitCooldown.Seconds()),
		}
	}
	return false, probeErr
}

type Server struct {
	mu                  sync.Mutex
	tokens              *auth.Store
	accountPool         *accountHealth
	accountConcurrency  *accountConcurrency
	pkce                map[string]pendingPKCE
	chat                *chathub.Client
	proxyClients        sync.Map
	sessions            *sessionStore
	userSessions        *userSessionStore
	sessionResolver     *sessionResolver
	conversationManager *conversationManager
	adminPassword       string
	adminSessions       map[string]time.Time
	mustChangePassword  bool
	loginAttempts       map[string]loginAttempt
	apiKeys             *apiKeyStore
	debug               *debugStore
	settings            *settingsStore
	responseMu          sync.Mutex
	responseMessages    map[string]map[string]respHistory
	usage               *usageLog
	generatedImages     map[string]generatedImage
	convCache           *conversationCache
}

const maxResponsesPerTenant = 256

func (s *Server) clientForProxy(proxyURL string) *chathub.Client {
	if proxyURL == "" {
		return s.chat
	}
	if v, ok := s.proxyClients.Load(proxyURL); ok {
		return v.(*chathub.Client)
	}
	clients, err := outbound.New(proxyURL)
	if err != nil {
		log.Printf("[bound-proxy] invalid proxy %q: %v", proxyURL, err)
		return s.chat
	}
	c := &chathub.Client{
		HTTPHeader: make(http.Header),
		HTTPClient: clients.HTTP,
		Dialer:     clients.WebSocket,
		Pool:       chathub.NewConnPool(clients.WebSocket, make(http.Header)),
		Trace:      s.chat.Trace,
	}
	c.HTTPHeader.Set("Origin", "https://m365.cloud.microsoft")
	c.HTTPHeader.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	actual, _ := s.proxyClients.LoadOrStore(proxyURL, c)
	return actual.(*chathub.Client)
}

type respHistory struct {
	At       time.Time
	Messages []oaiMsg
}

func New() (*Server, error) {
	store, err := auth.OpenStore("")
	if err != nil {
		return nil, err
	}
	password, mustChange := loadAdminPassword()
	sessionTTL := 30 * time.Minute
	if v := os.Getenv("M365_USER_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			sessionTTL = d
		}
	}
	return &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		accountConcurrency: newAccountConcurrency(),
		pkce:               map[string]pendingPKCE{},
		chat: func() *chathub.Client {
			c := chathub.NewClient()
			c.Trace = func(meta map[string]any) { fmt.Printf("[multimodal-trace] %s\\n", mustJSON(meta)) }
			return c
		}(),
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(sessionTTL),
		sessionResolver:     openSessionResolver(),
		conversationManager: openConversationManager(),
		adminPassword:       password,
		adminSessions:       map[string]time.Time{},
		mustChangePassword:  mustChange,
		loginAttempts:       map[string]loginAttempt{},
		apiKeys:             openAPIKeys(),
		debug:               openDebugStore(),
		settings:            openSettingsStore(),
		responseMessages:    map[string]map[string]respHistory{},
		usage:               openUsageLog(),
		generatedImages:     map[string]generatedImage{},
		convCache:           newConversationCache(),
	}, nil
}

func (s *Server) StartConvCacheGC() {
	go func() {
		for {
			time.Sleep(2 * time.Minute)
			s.convCache.GC()
		}
	}()
}

func (s *Server) InitM365CloudClient() {
	accounts := s.tokens.List()
	if len(accounts) == 0 {
		return
	}
	acc := accounts[0]
	clientID := os.Getenv("M365_CLIENT_ID")
	if clientID == "" {
		clientID = acc.ClientID
	}
	if clientID == "" {
		clientID = auth.DefaultClientID
	}
	InitM365CloudClient(clientID, acc.TID, acc.RefreshToken)
	log.Printf("[m365-cloud] client initialized for account %s", acc.Email)
}

func (s *Server) RefreshExpiredTokens() {
	results := s.tokens.RefreshAllExpired()
	for _, r := range results {
		if r.Success {
			log.Printf("[token-refresh] account=%s refreshed, expires=%s", r.Email, r.ExpiresAt.Format(time.RFC3339))
		} else {
			log.Printf("[token-refresh] account=%s failed: %s", r.Email, r.Error)
		}
	}
}

func (s *Server) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/api/admin/login", s.adminLogin)
	m.HandleFunc("/api/admin/logout", s.adminLogout)
	m.HandleFunc("/api/admin/session", s.adminSession)
	m.HandleFunc("/api/admin/change-password", s.adminChangePassword)
	m.HandleFunc("/api/admin/keys", s.adminKeys)
	m.HandleFunc("/api/admin/models", s.adminModels)
	m.HandleFunc("/api/admin/models/test", s.adminModelTest)
	m.HandleFunc("/api/admin/models/sync", s.adminModelSync)
	m.HandleFunc("/api/admin/settings", s.adminSettings)
	m.HandleFunc("/api/admin/proxy-pool", s.proxyPool)
	m.HandleFunc("/api/admin/deployments", s.deployments)
	m.HandleFunc("/api/admin/deployment", s.deploymentAction)
	m.HandleFunc("/api/admin/deployment/check", s.deploymentCheck)
	m.HandleFunc("/api/admin/debug/logs", s.debugList)
	m.HandleFunc("/api/admin/debug/detail", s.debugDetail)
	m.HandleFunc("/api/health", s.health)
	m.HandleFunc("/api/version", s.version)
	m.HandleFunc("/api/update", s.update)
	m.HandleFunc("/api/accounts", s.accounts)
	m.HandleFunc("/api/accounts/refresh", s.refreshAccount)
	m.HandleFunc("/api/accounts/schedule", s.scheduleAccount)
	m.HandleFunc("/api/accounts/token-health", s.tokenHealth)
	m.HandleFunc("/api/accounts/clear-cooldown", s.clearCooldown)
	m.HandleFunc("/api/accounts/delete", s.deleteAccount)
	m.HandleFunc("/api/accounts/provision", s.provisionAccount)
	m.HandleFunc("/api/accounts/bind-proxy", s.bindProxy)
	m.HandleFunc("/api/auth/start", s.startPKCE)
	m.HandleFunc("/api/auth/status", s.pkceStatus)
	m.HandleFunc("/api/auth/callback", s.callbackPKCE)
	m.HandleFunc("/api/chat", s.chatOnce)
	m.HandleFunc("/api/chat/stream", s.chatStream)
	m.HandleFunc("/api/conversations", s.conversations)
	m.HandleFunc("/api/conversations/delete", s.deleteConversation)
	m.HandleFunc("/api/conversations/cleanup", s.conversationCleanup)
	m.HandleFunc("/api/conversations/whitelist", s.conversationWhitelist)
	m.HandleFunc("/v1/sessions", s.handleSessions)
	m.HandleFunc("/v1/sessions/", s.handleSessionDelete)
	m.HandleFunc("/api/m365/conversations", s.handleM365Conversations)
	m.HandleFunc("/api/m365/conversations/detail", s.handleM365ConversationDetail)
	m.HandleFunc("/api/m365/conversations/delete", s.handleM365Delete)
	m.HandleFunc("/api/m365/conversations/cleanup", s.handleM365Cleanup)
	m.HandleFunc("/api/stats", s.handleCacheStats)
	m.HandleFunc("/api/stats/reset", s.handleCacheStatsReset)
	m.HandleFunc("/api/usage", s.adminUsage)
	m.HandleFunc("/api/usage/logs", s.adminUsageLogs)
	m.HandleFunc("/v1/models", s.openaiModels)
	m.HandleFunc("/v1/chat/completions", s.openaiChat)
	m.HandleFunc("/v1/responses", s.responses)
	m.HandleFunc("/v1/responses/compact", s.responsesCompact)
	m.HandleFunc("/v1/mcp/sse", mcp.HandleSSE)
	m.HandleFunc("/v1/mcp/message", mcp.HandleMessage)
	m.HandleFunc("/v1/mcp/tools", mcp.HandleToolsList)
	m.HandleFunc("/v1/messages", s.anthropicMessages)
	m.HandleFunc("/v1/images/generations", s.imageGenerations)
	m.HandleFunc("/v1/images/edits", s.imageEdits)
	m.HandleFunc("/v1/images/files/", s.generatedImageFile)
	m.HandleFunc("/", s.rootPage)
	return recoverPanics(requestID(httpTrace(securityHeaders(s.adminMiddleware(s.debugMiddleware(m))))))
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/images/files/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" || r.URL.Path == "/api/admin/logout" || r.URL.Path == "/api/auth/start" || r.URL.Path == "/api/auth/status" || r.URL.Path == "/api/auth/callback" || r.URL.Path == "/" || r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		if mcp.IsCapabilityRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			tenantID, ok := s.authenticateAPIKey(r)
			if !ok {
				http.Error(w, `{"error":{"message":"valid API key required","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), apiTenantContextKey{}, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if s.adminPassword == "" {
			http.Error(w, `{"error":{"message":"administrator password is not configured","type":"configuration_error"}}`, http.StatusServiceUnavailable)
			return
		}
		if !s.validAdminSession(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
			return
		}
		s.mu.Lock()
		mustChange := s.mustChangePassword
		s.mu.Unlock()
		if mustChange && r.URL.Path != "/api/admin/change-password" && r.URL.Path != "/api/admin/logout" {
			writeOpenAIError(w, http.StatusForbidden, "password_change_required", "administrator password must be changed before using the console")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureAdminCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Only trust X-Forwarded-Proto from a loopback reverse proxy.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return net.ParseIP(host).IsLoopback() && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) validAdminSession(r *http.Request) bool {
	c, err := r.Cookie("m365_admin_session")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.adminSessions[c.Value]
	if !ok || time.Now().After(expires) {
		delete(s.adminSessions, c.Value)
		return false
	}
	return true
}

const maxAdminSessions = 4096

// pruneAdminSessions drops expired entries; callers must hold s.mu.
func pruneAdminSessions(m map[string]time.Time, now time.Time) {
	for k, exp := range m {
		if now.After(exp) {
			delete(m, k)
		}
	}
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	ip, now := clientIP(r), time.Now()
	if ok, wait := s.loginAllowed(ip, now); !ok {
		seconds := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many failed login attempts; try again later")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	decodeErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	s.mu.Lock()
	password := s.adminPassword
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	if decodeErr != nil || body.Password == "" || subtle.ConstantTimeCompare([]byte(body.Password), []byte(password)) != 1 {
		s.recordLoginFailure(ip, now)
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "invalid administrator password")
		return
	}
	s.clearLoginFailures(ip)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeOpenAIError(w, 500, "internal_error", "session failure")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	pruneAdminSessions(s.adminSessions, now)
	if len(s.adminSessions) >= maxAdminSessions {
		// Evict the oldest entry to keep the map bounded.
		var oldest string
		var oldestExp time.Time
		for k, exp := range s.adminSessions {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(s.adminSessions, oldest)
	}
	s.adminSessions[token] = now.Add(24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Value: token, Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: 86400})
	jsonOut(w, map[string]any{"status": "authenticated", "must_change_password": mustChange})
}
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("m365_admin_session"); e == nil {
		s.mu.Lock()
		delete(s.adminSessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	jsonOut(w, map[string]string{"status": "logged_out"})
}
func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	authenticated := s.validAdminSession(r)
	s.mu.Lock()
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	jsonOut(w, map[string]bool{"authenticated": authenticated, "must_change_password": authenticated && mustChange})
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"keys": s.apiKeys.list()})
	case http.MethodPost:
		var b struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			http.Error(w, "bad json", 400)
			return
		}
		if strings.TrimSpace(b.Name) == "" {
			b.Name = "API key"
		}
		rec, raw, e := s.apiKeys.create(b.Name)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		jsonOut(w, map[string]any{"key": raw, "record": rec})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		deleted, e := s.apiKeys.delete(id)
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, "key not found", 404)
			return
		}
		jsonOut(w, map[string]string{"status": "deleted"})
	case http.MethodPut:
		var b struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Revoked *bool  `json:"revoked"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || b.ID == "" {
			http.Error(w, "bad json", 400)
			return
		}
		updated, e := s.apiKeys.update(b.ID, b.Name, b.Revoked)
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
		if !updated {
			http.Error(w, "key not found", 404)
			return
		}
		jsonOut(w, map[string]string{"status": "updated"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

type apiTenantContextKey struct{}

func requestAPICredential(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if raw == "" {
		v := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			raw = strings.TrimSpace(v[7:])
		}
	}
	return raw
}

func (s *Server) authenticateAPIKey(r *http.Request) (string, bool) {
	raw := requestAPICredential(r)
	if raw != "" && s.apiKeys != nil {
		if id, ok := s.apiKeys.authenticate(raw); ok {
			return "key:" + id, true
		}
	}
	return "", false
}

func (s *Server) validAPIKey(r *http.Request) bool {
	_, ok := s.authenticateAPIKey(r)
	return ok
}

func requestTenantID(r *http.Request) string {
	if tenantID, ok := r.Context().Value(apiTenantContextKey{}).(string); ok && tenantID != "" {
		return tenantID
	}
	if raw := requestAPICredential(r); raw != "" {
		return "credential:" + keyHash(raw)
	}
	// Internal/admin calls and direct handler tests do not pass through the
	// public API-key middleware. Keep them in an isolated local-only scope.
	return "local"
}

func tenantScopedID(tenantID, identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	return keyHash(tenantID + "\x00" + identity)
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	list := s.tokens.List()
	jsonOut(w, map[string]any{
		"status":             "ok",
		"auth":               []string{"pkce"},
		"chat":               "chathub",
		"clientId":           auth.ClientID(),
		"scope":              auth.Scope(),
		"tokenCache":         s.tokens.Path(),
		"accountCount":       len(list),
		"accountConcurrency": s.accountConcurrency.Snapshot(),
	})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list := s.tokens.List()
	type view struct {
		ID              string     `json:"id"`
		Email           string     `json:"email"`
		DisplayName     string     `json:"displayName,omitempty"`
		Status          string     `json:"status"`
		ScheduleEnabled bool       `json:"scheduleEnabled"`
		CallCount       uint64     `json:"callCount"`
		RateLimited     bool       `json:"rateLimited"`
		CooldownUntil   *time.Time `json:"cooldownUntil,omitempty"`
		OID             string     `json:"oid,omitempty"`
		TID             string     `json:"tid,omitempty"`
		ExpiresAt       time.Time  `json:"expiresAt,omitempty"`
		UpdatedAt       time.Time  `json:"updatedAt,omitempty"`
		BoundProxy      string     `json:"boundProxy,omitempty"`
	}
	out := make([]view, 0, len(list))
	for _, a := range list {
		status := a.Status
		var cooldownUntil *time.Time
		var callCount uint64
		var rateLimited bool
		if s.accountPool != nil {
			if until, ok := s.accountPool.CooldownUntil(a.ID); ok {
				status = "cooldown"
				cooldownUntil = &until
			}
			callCount = s.accountPool.CallCount(a.ID)
			rateLimited = s.accountPool.RateLimited(a.ID)
		}
		out = append(out, view{
			ID: a.ID, Email: a.Email, DisplayName: a.DisplayName,
			Status: status, ScheduleEnabled: !a.ScheduleDisabled, CallCount: callCount, RateLimited: rateLimited,
			CooldownUntil: cooldownUntil, OID: a.OID, TID: a.TID,
			ExpiresAt: a.ExpiresAt, UpdatedAt: a.UpdatedAt, BoundProxy: a.BoundProxy,
		})
	}
	jsonOut(w, map[string]any{"accounts": out, "health": s.accountPool.Snapshot()})
}

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	acc, err := s.tokens.EnsureValid(strings.TrimSpace(body.ID))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "refreshed", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt, "updatedAt": acc.UpdatedAt,
	}})
}

func (s *Server) scheduleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.tokens.SetScheduleEnabled(strings.TrimSpace(body.ID), body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonOut(w, map[string]any{"status": "updated", "scheduleEnabled": body.Enabled})
}

func (s *Server) tokenHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		results := s.tokens.RefreshAllExpired()
		refreshed, failed := 0, 0
		for _, r := range results {
			if r.Success {
				refreshed++
			} else {
				failed++
			}
		}
		jsonOut(w, map[string]any{"refreshed": refreshed, "failed": failed, "results": results})
		return
	}
	list := s.tokens.List()
	now := time.Now()
	type entry struct {
		ID        string    `json:"id"`
		Email     string    `json:"email"`
		Status    string    `json:"status"`
		ExpiresAt time.Time `json:"expires_at"`
		Expired   bool      `json:"expired"`
		ExpiresIn string    `json:"expires_in"`
	}
	out := make([]entry, 0, len(list))
	for _, a := range list {
		e := entry{ID: a.ID, Email: a.Email, Status: a.Status, ExpiresAt: a.ExpiresAt}
		if now.After(a.ExpiresAt) {
			e.Expired = true
			e.ExpiresIn = "expired"
		} else {
			e.ExpiresIn = a.ExpiresAt.Sub(now).Truncate(time.Second).String()
		}
		out = append(out, e)
	}
	jsonOut(w, map[string]any{"accounts": out, "now": now.Format(time.RFC3339)})
}

func (s *Server) clearCooldown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.accountPool.ClearAllCooldowns()
	jsonOut(w, map[string]any{"status": "ok"})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.tokens.Delete(body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]string{"status": "deleted"})
}

func (s *Server) provisionAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" || body.Password == "" {
		http.Error(w, "email and password required", http.StatusBadRequest)
		return
	}
	set, err := auth.ROPC(body.Email, body.Password)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "ropc_error", err.Error())
		return
	}
	acc, err := s.tokens.Upsert(set)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "upsert_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "provisioned", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt,
	}})
}

func (s *Server) bindProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		ID       string `json:"id"`
		ProxyURL string `json:"proxyUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if body.ProxyURL != "" {
		if err := outbound.ValidateProxyURL(body.ProxyURL); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
			return
		}
	}
	if err := s.tokens.SetBoundProxy(body.ID, body.ProxyURL); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	acc, _ := s.tokens.Get(body.ID)
	if acc.BoundProxy == "" {
		s.proxyClients.Range(func(key, _ any) bool {
			if keyStr, ok := key.(string); ok && keyStr != "" {
				s.proxyClients.Delete(keyStr)
			}
			return true
		})
	}
	jsonOut(w, map[string]any{"ok": true, "id": body.ID, "boundProxy": acc.BoundProxy})
}

func (s *Server) startPKCE(w http.ResponseWriter, _ *http.Request) {
	v, err := auth.Verifier()
	if err != nil {
		http.Error(w, "pkce failure", http.StatusInternalServerError)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "state failure", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(b)
	redirectURI := auth.RedirectURI()
	s.mu.Lock()
	s.pkce[state] = pendingPKCE{Verifier: v, Created: time.Now(), Status: "pending", RedirectURI: redirectURI}
	s.mu.Unlock()
	jsonOut(w, map[string]string{
		"status": "pkce_ready",
		"state":  state,
		"url": auth.AuthorizationURL(
			auth.AuthorizeEndpoint(),
			auth.ClientID(),
			redirectURI,
			state,
			auth.Challenge(v),
			auth.Scope(),
		),
		"redirectUri": redirectURI,
		"note":        "If redirect is nativeclient, paste the final URL/code into /api/auth/callback after login.",
	})
}

func (s *Server) pkceStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p, ok := s.pkce[state]
	if ok && time.Since(p.Created) > 10*time.Minute {
		delete(s.pkce, state)
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		jsonOut(w, map[string]any{"status": "expired"})
		return
	}
	out := map[string]any{"status": p.Status}
	if p.Account != nil {
		out["account"] = p.Account
	}
	if p.Error != "" {
		out["error"] = p.Error
	}
	jsonOut(w, out)
}

func (s *Server) callbackPKCE(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	oauthError := r.URL.Query().Get("error")
	// also accept pasted full callback URL
	if code == "" && oauthError == "" {
		if u := r.URL.Query().Get("url"); u != "" {
			if parsed, err := http.NewRequest(http.MethodGet, u, nil); err == nil {
				code = parsed.URL.Query().Get("code")
				oauthError = parsed.URL.Query().Get("error")
				if state == "" {
					state = parsed.URL.Query().Get("state")
				}
			}
		}
	}
	if state == "" || (code == "" && oauthError == "") {
		http.Error(w, "missing state or authorization result", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p, ok := s.pkce[state]
	if !ok || time.Since(p.Created) > 10*time.Minute {
		if ok {
			delete(s.pkce, state)
		}
		s.mu.Unlock()
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	if p.Status != "pending" {
		s.mu.Unlock()
		http.Error(w, "authorization result already consumed", http.StatusConflict)
		return
	}
	p.Status = "processing"
	s.pkce[state] = p
	s.mu.Unlock()
	if oauthError != "" {
		log.Printf("oauth_error stage=callback error=%q", oauthError)
		s.mu.Lock()
		p.Status = "error"
		p.Error = oauthError
		s.pkce[state] = p
		s.mu.Unlock()
		http.Error(w, "Microsoft authorization failed: "+oauthError, http.StatusBadRequest)
		return
	}
	redirectURI := p.RedirectURI
	if redirectURI == "" {
		redirectURI = auth.RedirectURI()
	}
	tok, err := auth.ExchangeCode(code, p.Verifier, redirectURI)
	if err != nil {
		logOAuthError("code_exchange", err)
		s.mu.Lock()
		p.Status = "error"
		p.Error = err.Error()
		s.pkce[state] = p
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	acc, err := s.tokens.Upsert(tok)
	if err != nil {
		s.mu.Lock()
		p.Status = "error"
		p.Error = err.Error()
		s.pkce[state] = p
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	p.Status = "authenticated"
	p.Account = map[string]any{"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName, "status": acc.Status, "oid": acc.OID, "tid": acc.TID}
	s.pkce[state] = p
	s.mu.Unlock()
	// Browser loopback callbacks should finish in a friendly page instead of
	// displaying a raw JSON response. Keep JSON for the manual/API flow.
	if strings.HasPrefix(redirectURI, "http://127.0.0.1:") || strings.HasPrefix(redirectURI, "http://localhost:") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>M365 Copilot2API 授权完成</title><style>body{font:16px system-ui;text-align:center;padding:15vh 20px;color:#242424}main{max-width:520px;margin:auto}h1{font-size:26px}</style><main><h1>授权完成</h1><p>账号已经自动加入账号池，可以关闭此页面。</p><script>if(window.opener){window.opener.postMessage({type:"m365-auth-complete"},window.location.origin);setTimeout(()=>window.close(),300)}</script></main>`)
		return
	}
	jsonOut(w, map[string]any{
		"status":  "authenticated",
		"account": map[string]any{"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName, "status": acc.Status, "oid": acc.OID, "tid": acc.TID},
	})
}

func (s *Server) resolveAccount(accountID string) (auth.AccountToken, error) {
	if accountID == "" {
		return s.bestScheduledAccount("")
	}
	return s.tokens.EnsureValid(accountID)
}

// bestScheduledAccount orders eligible accounts by current concurrency,
// recent time-to-first-delta and observed failures. Cooldown and scheduling
// controls remain hard filters.
func (s *Server) bestScheduledAccount(avoidID string) (auth.AccountToken, error) {
	accounts := s.tokens.List()
	if len(accounts) == 0 {
		return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
	}
	type scoredAccount struct {
		account auth.AccountToken
		score   float64
	}
	candidates := make([]scoredAccount, 0, len(accounts))
	enabled := false
	cooling := false
	saturated := false
	for _, candidate := range accounts {
		if candidate.ID == "" || candidate.ID == avoidID || candidate.ScheduleDisabled {
			continue
		}
		enabled = true
		if s.accountPool != nil && !s.accountPool.Available(candidate.ID) {
			cooling = true
			continue
		}
		if !s.accountConcurrency.Available(candidate.ID) {
			saturated = true
			continue
		}
		score := float64(s.accountConcurrency.Inflight(candidate.ID)) * 2000
		if s.accountPool != nil {
			score = s.accountPool.Score(candidate.ID, s.accountConcurrency.Inflight(candidate.ID))
		}
		candidates = append(candidates, scoredAccount{account: candidate, score: score})
	}
	if len(candidates) > 0 {
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score < candidates[j].score })
		var lastErr error
		for _, candidate := range candidates {
			validated, err := s.tokens.EnsureValid(candidate.account.ID)
			if err == nil {
				return validated, nil
			}
			lastErr = err
			if s.accountPool != nil {
				s.accountPool.Observe(candidate.account.ID, 0, err)
				message := strings.ToLower(err.Error())
				if strings.Contains(message, "token_expired") || strings.Contains(message, "invalid_grant") {
					s.accountPool.MarkFailure(candidate.account.ID, &UpstreamHTTPError{Status: http.StatusUnauthorized, Body: "account token refresh failed"}, 0)
				}
			}
		}
		return auth.AccountToken{}, fmt.Errorf("no account has valid credentials: %w", lastErr)
	}
	if !enabled {
		return auth.AccountToken{}, fmt.Errorf("no accounts enabled for scheduling")
	}
	if cooling {
		until := time.Now().Add(5 * time.Second)
		if s.accountPool != nil {
			until = s.accountPool.EarliestRecovery()
		}
		retry := int(time.Until(until).Seconds())
		if retry < 5 {
			retry = 5
		}
		return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: retry, Body: "all accounts are cooling down; try again later"}
	}
	if saturated {
		return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: 1, Body: "all accounts are at their concurrency limit; try again shortly"}
	}
	return auth.AccountToken{}, fmt.Errorf("no healthy account available")
}

// nextHealthyAccount returns the best-scoring healthy account other than the
// failed one and validates its token.
func (s *Server) nextHealthyAccount(avoidID string) (auth.AccountToken, error) {
	return s.bestScheduledAccount(avoidID)
}

type chatBody struct {
	AccountID      string               `json:"accountId"`
	Message        string               `json:"message"`
	Prompt         string               `json:"prompt"`
	Tone           string               `json:"tone"`
	ConversationID string               `json:"conversationId"`
	SessionID      string               `json:"sessionId"`
	SessionKey     string               `json:"sessionKey"`
	Attachments    []chathub.Attachment `json:"attachments,omitempty"`
	Tools          []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat   `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

func modelTone(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.2":
		return "Gpt_5_2_Chat"
	case "gpt-5.2-reasoning":
		return "Gpt_5_2_Reasoning"
	case "gpt-5.3":
		return "Gpt_5_3_Chat"
	case "gpt-5.4":
		return "Gpt_5_4_Chat"
	case "gpt-5.4-reasoning":
		return "Gpt_5_4_Reasoning"
	case "gpt-5.5":
		return "Gpt_5_5_Chat"
	case "gpt-5.5-reasoning":
		return "Gpt_5_5_Reasoning"
	case "gpt-5.6-reasoning":
		return "Gpt_5_6_Reasoning"
	case "claude", "claude-sonnet":
		return "Claude_Sonnet"
	case "claude-sonnet-reasoning":
		return "Claude_Sonnet_Reasoning"
	case "gpt-5.4-quick":
		return "Gpt_5_4_Chat"
	case "gpt-5.3-think-deeper":
		return "Gpt_5_3_Chat"
	default:
		return "magic"
	}
}

type streamDeliveryState struct {
	visible bool
}

func (s *streamDeliveryState) canRetry() bool {
	return s == nil || !s.visible
}

func (s *streamDeliveryState) markVisible(trace *usageTrace) {
	if s == nil || s.visible {
		return
	}
	s.visible = true
	trace.markFirstToken()
}

type openAIStreamEmitter struct {
	r        *http.Request
	w        http.ResponseWriter
	flusher  http.Flusher
	id       string
	model    string
	trace    *usageTrace
	delivery *streamDeliveryState
	mu       sync.Mutex
	first    bool
}

func newOpenAIStreamEmitter(r *http.Request, w http.ResponseWriter, flusher http.Flusher, id, model string, trace *usageTrace, delivery *streamDeliveryState) *openAIStreamEmitter {
	return &openAIStreamEmitter{r: r, w: w, flusher: flusher, id: id, model: model, trace: trace, delivery: delivery, first: true}
}

func openAIDeltaVisible(delta map[string]any) bool {
	for _, key := range []string{"content", "reasoning_content", "tool_calls"} {
		value, ok := delta[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			if text != "" {
				return true
			}
			continue
		}
		return true
	}
	return false
}

const sseKeepaliveInterval = 10 * time.Second

func (e *openAIStreamEmitter) writeRaw(payload string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writeRawLocked(payload)
}

func (e *openAIStreamEmitter) writeRawLocked(payload string) error {
	if err := e.r.Context().Err(); err != nil {
		return err
	}
	rc := http.NewResponseController(e.w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(e.w, payload); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

func (e *openAIStreamEmitter) startKeepalive(interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := e.writeRaw(": keepalive\n\n"); err != nil {
					return
				}
			case <-e.r.Context().Done():
				return
			case <-stop:
				return
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
}

func (e *openAIStreamEmitter) resetFirst() {
	e.mu.Lock()
	e.first = true
	e.mu.Unlock()
}

func (e *openAIStreamEmitter) writeDelta(delta map[string]any) error {
	if !openAIDeltaVisible(delta) {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.first {
		withRole := map[string]any{"role": "assistant", "content": nil}
		for key, value := range delta {
			withRole[key] = value
		}
		delta = withRole
	}
	chunk := map[string]any{"id": e.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": e.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
	err := e.writeRawLocked("data: " + mustJSON(chunk) + "\n\n")
	if err == nil {
		e.first = false
		e.delivery.markVisible(e.trace)
	}
	return err
}

type visibleToolResponseWriter struct {
	http.ResponseWriter
	trace    *usageTrace
	delivery *streamDeliveryState
}

func (w *visibleToolResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		payload := string(p[:n])
		if strings.Contains(payload, `"tool_calls"`) || strings.Contains(payload, `"reasoning_content"`) {
			w.delivery.markVisible(w.trace)
		}
	}
	return n, err
}

func (w *visibleToolResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeVisibleToolResponse(r *http.Request, w http.ResponseWriter, delivery *streamDeliveryState, id, model string, calls []detectedToolCall, res chathub.Result) error {
	trace := usageTraceFrom(r.Context())
	trace.setToolCalls(len(calls))
	return writeToolResponse(&visibleToolResponseWriter{ResponseWriter: w, trace: trace, delivery: delivery}, id, model, true, calls, res)
}

func streamErrorCode(err error) string {
	if chathub.IsStreamInterrupted(err) {
		return "stream_interrupted"
	}
	switch upstreamStatus(err) {
	case http.StatusTooManyRequests:
		return "rate_limit"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	default:
		return "upstream_error"
	}
}

func writeOpenAIStreamError(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, message, code string) {
	writeOpenAIStreamFailure(ctx, w, flusher, message, code, chathub.Result{})

}

func writeOpenAIStreamFailure(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, message, code string, result chathub.Result) {
	errorBody := map[string]any{"message": message, "code": code}
	if result.Incomplete {
		errorBody["partial"] = true
		errorBody["upstream_request_id"] = result.RequestID
		errorBody["failure_stage"] = result.FailureStage
		errorBody["last_delta_ms"] = result.LastDeltaMs
		if result.UpstreamCloseCode != 0 {
			errorBody["upstream_close_code"] = result.UpstreamCloseCode
		}
	}
	_ = sseRaw(ctx, w, flusher, "data: "+mustJSON(map[string]any{"error": errorBody})+"\n\n")
	_ = sseRaw(ctx, w, flusher, "data: [DONE]\n\n")
}

var errRequiredStreamToolCall = errors.New("model did not select a required tool")

func planRequiredStreamToolCall(ctx context.Context, routePrompt, tone string, attachments []chathub.Attachment, tools []map[string]any, choice any, ledger agentLedger, call func(context.Context, chathub.Request) (chathub.Result, error)) ([]detectedToolCall, chathub.Result, error) {
	result, err := call(withToolPlanning(ctx), chathub.Request{Text: routePrompt, Tone: tone, Attachments: attachments})
	if err != nil {
		return nil, result, err
	}
	calls, parsed := parseModelToolDecision(result.Text, tools, choice)
	if !parsed {
		repairPrompt := `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(result.Text, 6000)
		result, err = call(withRetryAttempt(withToolPlanning(ctx)), chathub.Request{Text: repairPrompt, Tone: tone, Attachments: attachments})
		if err != nil {
			return nil, result, err
		}
		calls, parsed = parseModelToolDecision(result.Text, tools, choice)
	}
	calls = filterCompletedCalls(calls, ledger)
	calls, _ = validateDetectedToolCalls(calls, tools, choice)
	if !parsed || len(calls) == 0 {
		return nil, result, errRequiredStreamToolCall
	}
	return calls, result, nil
}

func sseRaw(ctx context.Context, w http.ResponseWriter, f http.Flusher, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

func (s *Server) chatOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" && len(body.Attachments) == 0 {
		http.Error(w, "message or attachment required", http.StatusBadRequest)
		return
	}
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	acc, err := s.resolveAccount(body.AccountID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if claimsOID, claimsTID := extractOIDTID(acc.AccessToken); claimsOID != "" {
			acc.OID = claimsOID
			acc.TID = claimsTID
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid — re-login with PKCE browser client", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:           text,
		Tone:           body.Tone,
		ConversationID: body.ConversationID,
		SessionID:      body.SessionID,
		Attachments:    body.Attachments,
	})
	if err != nil {
		// Failover: a rate-limited or auth-failed account must not take down the
		// request when the pool has other healthy accounts. Only auto-selected
		// requests fail over; an explicitly chosen account is respected, and a
		// conversation-bound chat stays on its account.
		if body.AccountID == "" && body.ConversationID == "" && (IsRateLimited(err) || IsAuthFailure(err)) {
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr == nil {
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, chathub.Request{
					Text:           text,
					Tone:           body.Tone,
					ConversationID: body.ConversationID,
					SessionID:      body.SessionID,
					Attachments:    body.Attachments,
				})
				if err2 == nil {
					s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
					s.accountPool.MarkSuccess(next.ID)
					acc = next
					res = res2
					err = nil
				} else {
					err = err2
				}
			}
		}
		if err != nil {
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
			writeUpstreamError(w, err)
			return
		}
	}
	s.accountPool.MarkSuccess(acc.ID)
	res.Text = sanitizePublicAssistantText(res.Text)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: text})
	}
	jsonOut(w, map[string]any{
		"status":         "ok",
		"text":           res.Text,
		"conversationId": res.ConversationID,
		"sessionId":      res.SessionID,
		"requestId":      res.RequestID,
		"throttling":     res.Throttling,
		"result":         res.RawResult,
		"events":         res.Events,
		"images":         res.Images,
		"account":        map[string]any{"id": acc.ID, "email": acc.Email},
	})
}

// dropTransientConversation 异步删除 router/repair 轮创建的一次性云端对话，
// 避免每请求都往 M365 对话列表塞一条记录。删除失败不阻塞请求，留给 auto_cleanup 兜底。
func (s *Server) dropTransientConversation(conversationID string) {
	if conversationID == "" || m365CloudClient == nil {
		return
	}
	go func(id string) {
		if err := m365CloudClient.DeleteConversation(id); err != nil {
			log.Printf("[transient-conv] delete failed id=%s err=%v", id, err)
		}
	}(conversationID)
}

func (s *Server) adminModelSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	syncUpstreamTones()
	tones := liveUpstreamTones()
	jsonOut(w, map[string]any{"synced": true, "upstream_tones": tones, "count": len(tones)})
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jsonOut(w, map[string]any{"object": "list", "data": modelCatalog()})
}

// adminModelTest 由控制台模型测试调用，通过管理员会话鉴权，不依赖明文 API Key
// （密钥加固后 list 不再返回 raw，前端无法再自行携带 key 调用 /v1 端点）。
func (s *Server) adminModelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		Model string `json:"model"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Model) == "" {
		http.Error(w, "bad json: model required", http.StatusBadRequest)
		return
	}
	acc, err := s.resolveAccount("")
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "account_error", "account missing oid/tid")
		return
	}
	tone, _ := reasoningTone(b.Model, "")
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{
		Text: `Say "OK" in one word.`,
		Tone: tone,
	})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", upstreamError(err))
		return
	}
	jsonOut(w, map[string]any{"ok": true, "model": b.Model, "reply": sanitizePublicAssistantTextForModel(res.Text, b.Model), "latency_ms": ms})
}

func (s *Server) openaiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := modelCatalog()
	created := time.Now().Unix()
	for _, model := range data {
		model["created"] = created
	}
	// Codex v0.144.5 requires `models`, while OpenAI-compatible clients use
	// `data`. Keep both aliases backed by the same catalog.
	jsonOut(w, map[string]any{"object": "list", "data": data, "models": data})
}

type oaiMsg struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []map[string]any `json:"tool_calls,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type oaiReq struct {
	Model          string          `json:"model"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Messages       []oaiMsg        `json:"messages"`
	Stream         bool            `json:"stream"`
	// optional account routing
	User           string `json:"user"`
	AccountID      string `json:"accountId"`
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	SessionKey     string `json:"session_key"`
	// CamelCase aliases mirroring the response metadata fields; clients echo
	// m365.conversationId / m365.sessionId back verbatim.
	ConversationIDC string               `json:"conversationId,omitempty"`
	SessionIDC      string               `json:"sessionId,omitempty"`
	Attachments     []chathub.Attachment `json:"attachments,omitempty"`
	Tools           []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func contentToString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" || t == "input_text" || t == "output_text" {
					if s, _ := m["text"].(string); s != "" {
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func normalizeLegacyTools(body *oaiReq) {
	if len(body.Tools) == 0 && len(body.Functions) > 0 {
		body.Tools = make([]chathub.Tool, 0, len(body.Functions))
		for _, f := range body.Functions {
			body.Tools = append(body.Tools, chathub.Tool{Type: "function", Function: f})
		}
	}
	if body.ToolChoice == nil && body.FunctionCall != nil {
		body.ToolChoice = body.FunctionCall
	}
}

func toolChoiceDisablesTools(choice any) bool {
	return strings.EqualFold(strings.TrimSpace(normalizedToolChoiceMode(choice)), "none")
}

func normalizeRequestTools(body *oaiReq) {
	normalizeLegacyTools(body)
	if toolChoiceDisablesTools(body.ToolChoice) {
		body.Tools = nil
		body.Functions = nil
	}
}

func applyRequestSessionKey(body *oaiReq, r *http.Request) {
	if body.SessionKey == "" {
		body.SessionKey = strings.TrimSpace(r.Header.Get("X-M365-Session-ID"))
	}
}

func buildAnswerRequest(answerPrompt, tone string, body oaiReq, ledger agentLedger, planningMode string, mcpServerURL string) chathub.Request {
	if len(ledger.Completed) > 0 || len(ledger.Pending) > 0 {
		answerPrompt += "\n" + ledger.RouterContext()
	}
	if len(ledger.Completed) > 0 {
		answerPrompt += "\nFINAL ANSWER RULE: Report only actions supported by completed tool results. If the goal is not fully verified, state exactly what remains unconfirmed."
	}
	req := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments}
	if planningMode == "native" && !toolChoiceDisablesTools(body.ToolChoice) {
		req.Tools = body.Tools
		req.ToolChoice = body.ToolChoice
	}
	if mcpServerURL != "" && !toolChoiceDisablesTools(body.ToolChoice) {
		req.Tools = body.Tools
		if req.ToolChoice == nil {
			req.ToolChoice = body.ToolChoice
		}
		req.MCPServerURL = mcpServerURL
	}
	return req
}

func mcpToolsFromRequest(tools []chathub.Tool) []mcp.Tool {
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		var schema map[string]any
		if json.Unmarshal(f.Parameters, &schema) != nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, mcp.Tool{Name: f.Name, Description: f.Description, InputSchema: schema})
	}
	return out
}

func drainMCPToolCalls(queue *mcp.ToolCallQueue) []detectedToolCall {
	if queue == nil {
		return nil
	}
	var calls []detectedToolCall
	for {
		call := queue.DequeueNonBlocking()
		if call == nil {
			return calls
		}
		arguments, err := json.Marshal(call.Arguments)
		if err != nil {
			continue
		}
		calls = append(calls, detectedToolCall{ID: "call_" + uuid.NewString(), Name: call.Name, Arguments: arguments})
	}
}

func configuredMCPServerURL(scope string) (string, error) {
	raw := firstNonEmpty(os.Getenv("M365_MCP_PUBLIC_URL"), os.Getenv("M365_PUBLIC_URL"))
	if raw == "" {
		return "", fmt.Errorf("M365_MCP_PUBLIC_URL or M365_PUBLIC_URL must be configured when tools are enabled")
	}
	base, err := url.Parse(raw)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil {
		return "", fmt.Errorf("MCP public URL must be an absolute HTTPS URL without user info")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return "", fmt.Errorf("MCP public URL must not contain a query or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/mcp/sse"
	query := base.Query()
	query.Set("scope", scope)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func shouldEnforceCompletionEvidence(tools []map[string]any, active agentLedger) bool {
	return len(tools) > 0 && (len(active.Completed) > 0 || len(active.Pending) > 0)
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDFrom(r)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	startedAt := time.Now()
	r, trace := ensureUsageTrace(r, startedAt)
	usageWriter := &usageResponseWriter{ResponseWriter: w}
	w = usageWriter
	var body oaiReq
	var prompt string
	var acc auth.AccountToken
	log.Printf("[req-trace] id=%s stage=http_start stream=%t", requestID, r.URL.Query().Get("stream") == "true")
	defer func() {
		log.Printf("[req-trace] id=%s stage=http_return total_ms=%d", requestID, time.Since(startedAt).Milliseconds())
		metrics := trace.snapshot()
		if metrics.Recorded || isInternalUsageAdapter(r) {
			return
		}
		status := usageWriter.Status()
		if metrics.FailureStatus > 0 {
			status = metrics.FailureStatus
		}
		errorMessage := strings.TrimSpace(usageWriter.errorPreview.String())
		if metrics.FailureMessage != "" {
			errorMessage = metrics.FailureMessage
		}
		s.recordUsage(r, acc, UsageRecord{
			Model:        firstNonEmpty(body.Model, "m365-copilot"),
			Endpoint:     "/v1/chat/completions",
			Stream:       body.Stream,
			InputTokens:  EstimateTokens(prompt),
			ToolTokens:   int64(estimateToolTokens(body.Model, body.Tools, body.ToolChoice)),
			DurationMs:   time.Since(startedAt).Milliseconds(),
			Status:       status,
			ErrorType:    firstNonEmpty(metrics.FailureType, usageErrorType(status)),
			ErrorMessage: sanitizePublicInternalText(errorMessage),
		})
	}()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const maxChatRequestBody = 10 << 20
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	responseFormat := body.ResponseFormat
	effort := body.ReasoningEffort
	if body.Reasoning != nil && strings.TrimSpace(body.Reasoning.Effort) != "" {
		effort = body.Reasoning.Effort
	}
	tone, toneErr := reasoningTone(body.Model, effort)
	if toneErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", toneErr.Error())
		return
	}
	normalizeRequestTools(&body)
	body.ConversationID = firstNonEmpty(body.ConversationID, body.ConversationIDC)
	body.SessionID = firstNonEmpty(body.SessionID, body.SessionIDC)
	applyRequestSessionKey(&body, r)
	tenantID := requestTenantID(r)
	sessionLookupKey := tenantScopedID(tenantID, body.SessionKey)
	userLookupKey := tenantScopedID(tenantID, body.User)
	log.Printf("[req-trace] id=%s stage=body_parsed messages=%d tools=%d choice=%s raw_bytes=%d", requestID, len(body.Messages), len(body.Tools), normalizedToolChoiceMode(body.ToolChoice), len(raw))
	if err := validateToolConversation(body.Messages); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "tool_protocol_error", err.Error())
		return
	}
	// Tool evidence and round limits apply only to the current user turn. Older
	// calls may legitimately be repeated in a later turn with the same arguments.
	activeLedger := buildAgentLedger(activeMessages(body.Messages))
	if err := activeLedger.CanContinue(maxToolRounds()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "tool_round_limit", "message": err.Error(), "completed_calls": len(activeLedger.Completed)}})
		return
	}
	// Preserve role boundaries when adapting OpenAI messages to ChatHub's
	// single message.text field. This keeps system/developer instructions,
	// history, and the current user turn distinguishable.
	prompt, body.Attachments = flattenPromptMessages(body.Messages, body.Attachments)
	log.Printf("[req-trace] id=%s stage=prompt_flattened prompt_len=%d attachments=%d", requestID, len(prompt), len(body.Attachments))
	fmt.Printf("[multimodal-entry] messages=%d attachments=%d prompt_len=%d\n", len(body.Messages), len(body.Attachments), len(prompt))
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}
	if answer, ok := publicIdentityAnswer(body.Messages, body.Model); ok && responseFormat == nil {
		s.writePublicIdentityChatResponse(w, r, &body, prompt, answer, startedAt)
		return
	}

	if sessionLookupKey != "" {
		if v, ok := s.sessions.get(sessionLookupKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	if userLookupKey != "" && body.ConversationID == "" {
		if us, ok := s.userSessions.Get(userLookupKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, us.AccountID)
			body.ConversationID = us.ConversationID
			body.SessionID = us.SessionID
			log.Printf("[user-session] hit user=%s conversation=%s session=%s", body.User, us.ConversationID, us.SessionID)
		}
	}
	// 内容键会话复用：命中后云端对话已存全量历史，只需把客户端新增的
	// 消息拼成增量 prompt 发送（对齐 DeepSeek 上下文缓存语义）。
	answerPrompt := prompt
	contextReused := false
	resolvedConversationID := ""
	if body.ConversationID == "" && len(body.Messages) > 0 {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			resolvedConversationID = resolved.ConversationID
			body.ConversationID = resolved.ConversationID
			body.SessionID = resolved.SessionID
			body.AccountID = firstNonEmpty(body.AccountID, resolved.AccountID)
			log.Printf("[session-resolver] matched=%s conversation=%s history=%d total=%d", resolved.MatchedBy, resolved.ConversationID, resolved.HistoryLen, len(body.Messages))
			if resolved.HistoryLen > 0 && resolved.HistoryLen < len(body.Messages) {
				incPrompt, incAtt := flattenPromptMessages(body.Messages[resolved.HistoryLen:], nil)
				incPrompt = strings.TrimSpace(incPrompt)
				if incPrompt != "" {
					answerPrompt = incPrompt
					body.Attachments = incAtt
					contextReused = true
				}
			}
		}
	}
	accountID := body.AccountID
	acc, err = s.resolveAccount(accountID)
	if err != nil {
		log.Printf("[account-route] resolve failed requested=%q err=%v", accountID, err)
		writeUpstreamError(w, err)
		return
	}
	log.Printf("[account-route] selected id=%q email=%q token_present=%t oid_present=%t tid_present=%t", acc.ID, acc.Email, acc.AccessToken != "", acc.OID != "", acc.TID != "")
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", http.StatusBadRequest)
		return
	}

	// Conversation cache: reuse existing M365 conversation for same account+model
	// to avoid re-processing full system prompt + history each request (latency
	// drops from 3-5s to ~1s). Only kicks in when no explicit conversation ID
	// was provided by client, session key, user session, or session resolver.
	convReused := false
	convCacheModel := firstNonEmpty(body.Model, "m365-copilot")
	convCacheScope := conversationCacheScope(r, body)
	if convCacheScope != "" && body.ConversationID == "" && len(body.Messages) > 1 {
		sysHash := systemPromptHash(body.Messages)
		if cached := s.convCache.Lookup(convCacheScope, acc.ID, convCacheModel); cached != nil && cached.SystemPrompt == sysHash {
			if len(body.Messages) > cached.MessageCount {
				incPrompt, incAtt := flattenPromptMessages(body.Messages[cached.MessageCount:], nil)
				incPrompt = strings.TrimSpace(incPrompt)
				if incPrompt != "" {
					body.ConversationID = cached.ConversationID
					body.SessionID = cached.SessionID
					answerPrompt = incPrompt
					body.Attachments = incAtt
					convReused = true
					contextReused = true
					log.Printf("[conv-cache] hit account=%s model=%s conversation=%s cached_msgs=%d new_msgs=%d", acc.ID, convCacheModel, cached.ConversationID, cached.MessageCount, len(body.Messages))
				}
			}
		}
	}
	if convCacheScope != "" && !convReused && body.ConversationID == "" {
		log.Printf("[conv-cache] miss account=%s model=%s", acc.ID, convCacheModel)
	}

	// Normalize tools once. Selection is always made by the upstream model;
	// the gateway only validates its structured decision and converts protocols.
	toolMaps := make([]map[string]any, 0, len(body.Tools))
	for _, tool := range body.Tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		toolMaps = append(toolMaps, map[string]any{"type": tool.Type, "function": f})
	}
	if body.ToolChoice == nil && len(toolMaps) > 0 {
		body.ToolChoice = "auto"
	}
	var mcpServerURL string
	var mcpCallQueue *mcp.ToolCallQueue
	if len(toolMaps) > 0 {
		mcpTools := mcpToolsFromRequest(body.Tools)
		if len(mcpTools) > 0 {
			toolScope := "tools-" + uuid.NewString()
			mcpServerURL, err = configuredMCPServerURL(toolScope)
			if err != nil {
				writeOpenAIError(w, http.StatusServiceUnavailable, "mcp_configuration_error", err.Error())
				return
			}
			mcpCallQueue = mcp.NewToolCallQueue()
			mcp.GlobalToolRegistry.RegisterProviderForScope(toolScope, mcpTools, mcp.NewMCPToolProvider(mcpTools, mcpCallQueue))
			defer mcp.GlobalToolRegistry.ClearScope(toolScope)
			log.Printf("[mcp] tools=%d configured_gateway=true", len(toolMaps))
		}
	}
	validateCalls := func(stage string, calls []detectedToolCall) ([]detectedToolCall, int) {
		valid, rejected := validateDetectedToolCalls(calls, toolMaps, body.ToolChoice)
		for _, call := range rejected {
			log.Printf("[tool-validation] id=%s stage=%s rejected_name=%q reason=%q", requestID, stage, call.Name, call.Reason)
		}
		return valid, len(rejected)
	}
	planningMode := s.settings.get().ToolPlanningMode

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
	streamDelivery := &streamDeliveryState{}
	streamRouterAttempted := false
	// The stream is opened by the actual response path below. Do not emit a
	// tool preamble here: a request may contain tools in its schema while still
	// being an ordinary text question.
	// Streaming requests must not wait for the synchronous tool router. This
	// path forwards ordinary upstream text deltas immediately; tool routing for
	// non-streaming requests remains below until the event-level tool protocol
	// is available end-to-end.
	if planningMode == "router" && body.Stream && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		streamRouterAttempted = true
		// Preserve the existing validated tool router for streaming tool turns.
		// Only fall through to text streaming when the router explicitly selects
		// no tool; this prevents a natural-language preamble from becoming a
		// completed assistant turn with the actual call lost.
		routePrompt := modelToolRouterPrompt(answerPrompt+"\n"+activeLedger.RouterContext(), toolMaps, body.ToolChoice)
		log.Printf("[req-trace] id=%s stage=router_start prompt_len=%d", requestID, len(routePrompt))
		routeRes, routeErr := s.chatWithAccount(withToolPlanning(ctx), acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments})
		log.Printf("[req-trace] id=%s stage=router_return elapsed_ms=%d err=%t", requestID, time.Since(startedAt).Milliseconds(), routeErr != nil)
		// Router turns run in a throwaway cloud conversation that is never
		// reused by the answer turn; delete it so the conversation list does
		// not accumulate one entry per routed request.
		if routeErr == nil && routeRes.ConversationID != "" {
			s.dropTransientConversation(routeRes.ConversationID)
		}
		if routeErr != nil {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "stream unsupported", http.StatusInternalServerError)
				return
			}
			message := sanitizePublicInternalText(upstreamError(routeErr))
			if IsRateLimited(routeErr) {
				message = "upstream is rate limiting; try again shortly"
			}
			trace.fail(upstreamStatus(routeErr), usageErrorType(upstreamStatus(routeErr)), message)
			writeOpenAIStreamError(r.Context(), w, flusher, message, streamErrorCode(routeErr))
			return
		}
		calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		calls = filterCompletedCalls(calls, activeLedger)
		calls, _ = validateCalls("router", calls)
		if !parsed {
			repairRes, repairErr := s.chatWithAccount(withRetryAttempt(withToolPlanning(ctx)), acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments})
			if repairErr == nil && repairRes.ConversationID != "" {
				s.dropTransientConversation(repairRes.ConversationID)
			}
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				calls = filterCompletedCalls(calls, activeLedger)
				calls, _ = validateCalls("router", calls)
			}
		}
		if parsed && len(calls) > 0 {
			scope := fmt.Sprintf("%d:%v:stream", len(body.Messages), completedCallIDs(activeLedger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeVisibleToolResponse(r, w, streamDelivery, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), calls, routeRes)
			return
		}
	}
	if body.Stream {
		answerReq := buildAnswerRequest(answerPrompt, tone, body, activeLedger, planningMode, mcpServerURL)
		answerPrompt = answerReq.Text
		log.Printf("[req-trace] id=%s stage=answer_start prompt_len=%d native_tools=%d mcp=%s", requestID, len(answerPrompt), len(answerReq.Tools), mcpServerURL)
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		var text strings.Builder
		var delivered strings.Builder
		var pending strings.Builder
		var streamedTools []detectedToolCall
		identityFilter := newPublicIdentityStreamFilter(model)
		reasoningFilter := newPublicReasoningStreamFilter()
		emitter := newOpenAIStreamEmitter(r, w, flusher, id, model, trace, streamDelivery)
		if err := emitter.writeRaw(": connected\n\n"); err != nil {
			return
		}
		stopKeepalive := emitter.startKeepalive(sseKeepaliveInterval)
		defer stopKeepalive()
		requiredTool := strings.EqualFold(strings.TrimSpace(normalizedToolChoiceMode(body.ToolChoice)), "required")
		hadToolEvent := false
		invalidToolEvent := false
		writeText := func(part string) error {
			if part == "" {
				return nil
			}
			if err := emitter.writeDelta(map[string]any{"content": part}); err != nil {
				return err
			}
			delivered.WriteString(part)
			return nil
		}
		emitText := func(part string) error {
			if part == "" {
				return nil
			}
			part = identityFilter.Push(part)
			return writeText(part)
		}
		emitReasoning := func(part string) error {
			if part = reasoningFilter.Push(part); part == "" {
				return nil
			}
			return emitter.writeDelta(map[string]any{"reasoning_content": part})
		}
		handleEvent := func(ev chathub.StreamEvent) error {
			switch ev.Kind {
			case "tool":
				hadToolEvent = true
				if ev.ToolName != "" && len(ev.Arguments) > 0 {
					streamedTools = append(streamedTools, detectedToolCall{ID: "call_" + uuid.NewString(), Name: ev.ToolName, Arguments: ev.Arguments})
				} else {
					invalidToolEvent = true
				}
				return nil
			case "reasoning":
				return emitReasoning(ev.Text)
			case "text":
				if ev.Text == "" {
					return nil
				}
			default:
				return nil
			}
			text.WriteString(ev.Text)
			pending.WriteString(ev.Text)
			v := pending.String()
			if requiredTool {
				return nil
			}
			// If the text contains a bash block or a JSON command, don't emit it as text
			// It will be caught by fencedToolCalls after the stream completes
			if strings.Contains(v, "```bash") || strings.Contains(v, "\"command\"") {
				return nil
			}
			if i := strings.Index(v, "```"); i >= 0 {
				if err := emitText(v[:i]); err != nil {
					return err
				}
				pending.Reset()
				pending.WriteString(v[i:])
				return nil
			}
			if runeCount := utf8.RuneCountInString(v); runeCount > 8 {
				cut := 0
				seen := 0
				for i := range v {
					if seen == runeCount-8 {
						cut = i
						break
					}
					seen++
				}
				if err := emitText(v[:cut]); err != nil {
					return err
				}
				pending.Reset()
				pending.WriteString(v[cut:])
			}
			return nil
		}
		resetAttempt := func() {
			text.Reset()
			pending.Reset()
			streamedTools = nil
			hadToolEvent = false
			invalidToolEvent = false
			identityFilter = newPublicIdentityStreamFilter(model)
			reasoningFilter = newPublicReasoningStreamFilter()
			emitter.resetFirst()
		}
		res, err := s.chatWithAccountEvents(ctx, acc.ID, account, answerReq, handleEvent)
		if err != nil && streamDelivery.canRetry() && body.AccountID == "" && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err)) {
			// A throttled stream may retry on the next healthy account: only the
			// ": connected" preamble reached the client, so the retried stream is
			// indistinguishable from a fresh request.
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr != nil {
				// no healthy alternative
			} else {
				failoverReq := answerReq
				if body.ConversationID == resolvedConversationID {
					failoverReq.ConversationID = ""
					failoverReq.SessionID = ""
				}
				resetAttempt()
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccountEvents(withRetryAttempt(ctx2), next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq, handleEvent)
				if err2 == nil {
					res = res2
					acc = next
					err = nil
				} else {
					err = err2
					s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown)
				}
			}
		}
		if err != nil && chathub.IsStreamInterrupted(err) && delivered.Len() > 0 && !requiredTool && !hadToolEvent {
			resumeAttempts := s.settings.get().StreamResumeAttempts
			partialResult := res
			for attempt := 1; attempt <= resumeAttempts && r.Context().Err() == nil; attempt++ {
				resumePrompt := streamResumePrompt(answerPrompt, delivered.String())
				log.Printf("[req-trace] id=%s stage=stream_resume_start attempt=%d upstream_request_id=%s partial_chars=%d failure_stage=%s close_code=%d", requestID, attempt, partialResult.RequestID, utf8.RuneCountInString(delivered.String()), partialResult.FailureStage, partialResult.UpstreamCloseCode)
				resumeCtx, resumeCancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				resumeRes, resumeErr := s.chatWithAccount(withRetryAttempt(resumeCtx), acc.ID, account, chathub.Request{Text: resumePrompt, Tone: tone})
				resumeCancel()
				if resumeErr != nil {
					log.Printf("[req-trace] id=%s stage=stream_resume_error attempt=%d err=%v", requestID, attempt, resumeErr)
					err = resumeErr
					continue
				}
				continuation, overlap := trimStreamResumeOverlap(delivered.String(), sanitizePublicAssistantTextForModel(resumeRes.Text, body.Model))
				identityFilter = newPublicIdentityStreamFilter(model)
				reasoningFilter = newPublicReasoningStreamFilter()
				pending.Reset()
				if writeErr := emitText(continuation); writeErr != nil {
					log.Printf("[req-trace] id=%s stage=stream_resume_write attempt=%d err=%v", requestID, attempt, writeErr)
					return
				}
				if content := identityFilter.Flush(); content != "" {
					if writeErr := writeText(content); writeErr != nil {
						log.Printf("[req-trace] id=%s stage=stream_resume_write attempt=%d err=%v", requestID, attempt, writeErr)
						return
					}
				}
				text.Reset()
				text.WriteString(delivered.String())
				resumeRes.Text = delivered.String()
				resumeRes.Incomplete = false
				resumeRes.FailureStage = ""
				resumeRes.UpstreamCloseCode = 0
				res = resumeRes
				err = nil
				trace.markStreamRecovered(resumeRes.RequestID, resumeRes.LastDeltaMs)
				log.Printf("[req-trace] id=%s stage=stream_resume_success attempt=%d upstream_request_id=%s overlap_runes=%d continuation_chars=%d", requestID, attempt, resumeRes.RequestID, overlap, utf8.RuneCountInString(continuation))
				break
			}
			if err != nil {
				res = partialResult
				trace.markStreamPartial(partialResult)
			}
		}
		if err != nil {
			log.Printf("[req-trace] id=%s stage=stream_error upstream_request_id=%s failure_stage=%s close_code=%d partial=%t last_delta_ms=%d err=%v", requestID, res.RequestID, res.FailureStage, res.UpstreamCloseCode, res.Incomplete, res.LastDeltaMs, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
			if convReused {
				s.invalidateConvCache(convCacheScope, acc.ID, convCacheModel)
			}
			msg := upstreamError(err)
			if IsRateLimited(err) {
				msg = "upstream is rate limiting; try again shortly"
			}
			msg = sanitizePublicInternalText(msg)
			failureType := usageErrorType(upstreamStatus(err))
			if res.Incomplete {
				failureType = "stream_interrupted"
			}
			trace.fail(upstreamStatus(err), failureType, msg)
			code := streamErrorCode(err)
			if res.Incomplete {
				code = "stream_interrupted"
			}
			stopKeepalive()
			writeOpenAIStreamFailure(r.Context(), w, flusher, msg, code, res)
			return
		}
		s.accountPool.MarkSuccess(acc.ID)
		if text.Len() == 0 && strings.TrimSpace(res.Text) != "" {
			text.WriteString(res.Text)
			pending.WriteString(res.Text)
		}
		rawCalls := drainMCPToolCalls(mcpCallQueue)
		if len(rawCalls) == 0 {
			rawCalls = streamedTools
		}
		if len(rawCalls) == 0 {
			rawCalls = fencedToolCalls(text.String(), toolMaps, body.ToolChoice)
		}
		calls, rejected := validateCalls("stream", rawCalls)
		calls = filterCompletedCalls(calls, activeLedger)
		toolResult := chathub.Result{Text: text.String()}
		recoveryAttempted := false
		var recoveryErr error
		if len(calls) == 0 && (rejected > 0 || invalidToolEvent) {
			recoveryAttempted = true
			// A native ChatHub event can contain a fabricated or empty tool name.
			// Do not leak it to the local runner: ask the model to remap the intent
			// to exactly one of the tools the client actually declared.
			repairPrompt := modelToolRouterPrompt(prompt+"\n"+activeLedger.RouterContext(), toolMaps, "required") +
				"\nREPAIR RULE: The previous upstream event selected an undeclared tool. Select one declared tool that performs the intended operation. Never return unknown_tool."
			repairRes, repairErr := s.chatWithAccount(withRetryAttempt(withToolPlanning(ctx)), acc.ID, account, chathub.Request{Text: repairPrompt, Tone: tone, Attachments: body.Attachments})
			if repairErr == nil {
				repaired, parsed := parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				if parsed {
					calls, _ = validateCalls("stream-repair", repaired)
					if len(calls) > 0 {
						toolResult = repairRes
					}
				}
			}
			recoveryErr = repairErr
			if len(calls) == 0 {
				log.Printf("[tool-validation] id=%s stage=stream-repair failed", requestID)
			}
		}
		if len(calls) == 0 && requiredTool && !recoveryAttempted && !hadToolEvent && !streamRouterAttempted {
			recoveryAttempted = true
			routePrompt := modelToolRouterPrompt(prompt+"\n"+activeLedger.RouterContext(), toolMaps, "required")
			planner := func(planCtx context.Context, request chathub.Request) (chathub.Result, error) {
				planned, planErr := s.chatWithAccount(planCtx, acc.ID, account, request)
				if planErr == nil && planned.ConversationID != "" {
					s.dropTransientConversation(planned.ConversationID)
				}
				return planned, planErr
			}
			plannedCalls, plannedResult, planErr := planRequiredStreamToolCall(ctx, routePrompt, tone, body.Attachments, toolMaps, body.ToolChoice, activeLedger, planner)
			if planErr == nil {
				calls = plannedCalls
				toolResult = plannedResult
			} else {
				recoveryErr = planErr
			}
		}
		if len(calls) == 0 && requiredTool {
			status := http.StatusBadGateway
			code := "tool_protocol_error"
			message := "model did not select a required tool after one constrained routing attempt"
			if recoveryErr != nil && !errors.Is(recoveryErr, errRequiredStreamToolCall) {
				status = upstreamStatus(recoveryErr)
				code = streamErrorCode(recoveryErr)
				message = sanitizePublicInternalText(upstreamError(recoveryErr))
			}
			trace.fail(status, code, message)
			stopKeepalive()
			writeOpenAIStreamError(r.Context(), w, flusher, message, code)
			return
		}
		if len(calls) == 0 && recoveryAttempted {
			message := "upstream selected an undeclared tool and repair failed"
			trace.fail(http.StatusBadGateway, "invalid_tool_call", message)
			stopKeepalive()
			writeOpenAIStreamError(r.Context(), w, flusher, message, "invalid_tool_call")
			return
		}
		if len(calls) > 0 {
			log.Printf("[req-trace] id=%s stage=tool_calls_detected count=%d names=%v", requestID, len(calls), func() []string {
				var n []string
				for _, c := range calls {
					n = append(n, c.Name)
				}
				return n
			}())
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			stopKeepalive()
			_ = writeVisibleToolResponse(r, w, streamDelivery, id, model, calls, toolResult)
			if userLookupKey != "" && res.ConversationID != "" {
				s.userSessions.Put(userLookupKey, res.ConversationID, res.SessionID, acc.ID)
			}
			s.bindConversation(acc, &body, r, res, prompt, answerPrompt, contextReused, startedAt)
			s.storeConvCache(convCacheScope, acc.ID, convCacheModel, res, tone, body.Messages, convReused)
			return
		}
		if err := emitText(pending.String()); err != nil {
			log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
			return
		}
		if content := identityFilter.Flush(); content != "" {
			if err := writeText(content); err != nil {
				log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
				return
			}
		}
		if reasoning := reasoningFilter.Flush(); reasoning != "" {
			if err := emitter.writeDelta(map[string]any{"reasoning_content": reasoning}); err != nil {
				log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
				return
			}
		}
		finishChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
		stopKeepalive()
		_ = emitter.writeRaw("data: " + mustJSON(finishChunk) + "\n\n")
		_ = emitter.writeRaw("data: [DONE]\n\n")
		if userLookupKey != "" && res.ConversationID != "" {
			s.userSessions.Put(userLookupKey, res.ConversationID, res.SessionID, acc.ID)
		}
		s.bindConversation(acc, &body, r, res, prompt, answerPrompt, contextReused, startedAt)
		s.storeConvCache(convCacheScope, acc.ID, convCacheModel, res, tone, body.Messages, convReused)
		return
	}
	// Ask the upstream model to select and validate the next tool. The gateway
	// remains tool-agnostic; it only validates and serializes the decision.
	if planningMode == "router" && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routePrompt := modelToolRouterPrompt(answerPrompt+"\n"+activeLedger.RouterContext(), toolMaps, body.ToolChoice)
		routeRes, routeErr := s.chatWithAccount(withToolPlanning(ctx), acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments})
		if routeErr != nil {
			s.accountPool.MarkFailure(acc.ID, routeErr, rateLimitCooldown)
			if IsRateLimited(routeErr) || IsAuthFailure(routeErr) {
				next, nerr := s.nextHealthyAccount(acc.ID)
				if nerr == nil {
					ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
					defer cancel2()
					if res2, err2 := s.chatWithAccount(withRetryAttempt(withToolPlanning(ctx2)), next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments}); err2 == nil {
						routeRes, routeErr = res2, nil
						acc = next
						account = chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}
					} else {
						s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown)
					}
				}
			}
			if routeErr != nil {
				msg := upstreamError(routeErr)
				if IsRateLimited(routeErr) {
					msg = "upstream is rate limiting; try again shortly"
				}
				writeOpenAIError(w, http.StatusBadGateway, "tool_router_error", msg)
				return
			}
			s.accountPool.MarkSuccess(acc.ID)
		}
		calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		if !parsed {
			repairRes, repairErr := s.chatWithAccount(withRetryAttempt(withToolPlanning(ctx)), acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Do not invent calls; use {"calls":[]} if unrecoverable. OUTPUT:
` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments})
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
			}
			if !parsed {
				http.Error(w, "model returned an invalid tool routing decision", http.StatusBadGateway)
				return
			}
		}
		calls = filterCompletedCalls(calls, activeLedger)
		calls, _ = validateCalls("router", calls)
		if len(calls) > 0 {
			scope := fmt.Sprintf("%d:%v", len(body.Messages), completedCallIDs(activeLedger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponseTracked(r, w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, calls, routeRes)
			return
		}
		if fmt.Sprint(body.ToolChoice) == "required" {
			defs, _ := json.Marshal(toolMaps)
			retryText := `Select at least one required next tool call from FUNCTION_DEFINITIONS. Validate every argument against its schema. Return JSON only as {"calls":[{"name":"function_name","arguments":{}}]}.
APPLICATION_REQUEST_AND_EVIDENCE:
` + prompt + "\n" + activeLedger.RouterContext() + "\nFUNCTION_DEFINITIONS:\n" + string(defs)
			retryRes, retryErr := s.chatWithAccount(withRetryAttempt(withToolPlanning(ctx)), acc.ID, account, chathub.Request{Text: retryText, Tone: tone, Attachments: body.Attachments})
			if retryErr == nil {
				calls, parsed = parseModelToolDecision(retryRes.Text, toolMaps, body.ToolChoice)
				calls = filterCompletedCalls(calls, activeLedger)
				calls, _ = validateCalls("router", calls)
				if parsed && len(calls) > 0 {
					scope := fmt.Sprintf("%d:%v:required-retry", len(body.Messages), completedCallIDs(activeLedger))
					for i := range calls {
						calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
					}
					calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
					_ = writeToolResponseTracked(r, w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, calls, retryRes)
					return
				}
			}
			http.Error(w, "model did not select a required tool after constrained retry", http.StatusBadGateway)
			return
		}
	}
	answerReq := buildAnswerRequest(answerPrompt, tone, body, activeLedger, planningMode, mcpServerURL)
	answerPrompt = answerReq.Text
	var res chathub.Result
	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		emitter := newOpenAIStreamEmitter(r, w, flusher, id, model, trace, streamDelivery)
		if err := emitter.writeRaw(": connected\n\n"); err != nil {
			return
		}
		stopKeepalive := emitter.startKeepalive(sseKeepaliveInterval)
		defer stopKeepalive()
		writeChunk := emitter.writeDelta
		contentFilter := newPublicIdentityStreamFilter(firstNonEmpty(body.Model, defaultPublicModelName))
		reasoningFilter := newPublicReasoningStreamFilter()
		onDelta := func(content string) error {
			if content = contentFilter.Push(content); content != "" {
				return writeChunk(map[string]any{"content": content})
			}
			return nil
		}
		onReasoning := func(reasoning string) error {
			if reasoning = reasoningFilter.Push(reasoning); reasoning != "" {
				return writeChunk(map[string]any{"reasoning_content": reasoning})
			}
			return nil
		}
		res, err = s.chatWithAccountReasoning(ctx, acc.ID, account, answerReq, onDelta, onReasoning)
		if err != nil && streamDelivery.canRetry() && body.AccountID == "" && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err)) {
			// Retry a throttled stream on the next healthy account; the client
			// has only seen the ": connected" preamble so far, so the retry is
			// indistinguishable from a fresh request.
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr == nil {
				failoverReq := answerReq
				if body.ConversationID == resolvedConversationID {
					failoverReq.ConversationID = ""
					failoverReq.SessionID = ""
				}
				contentFilter = newPublicIdentityStreamFilter(firstNonEmpty(body.Model, defaultPublicModelName))
				reasoningFilter = newPublicReasoningStreamFilter()
				emitter.resetFirst()
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				if res2, err2 := s.chatWithAccountReasoning(withRetryAttempt(ctx2), next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq, onDelta, onReasoning); err2 == nil {
					res = res2
					acc = next
					err = nil
				} else {
					err = err2
					s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown)
				}
			}
		}
		if err == nil {
			if content := contentFilter.Flush(); content != "" {
				if writeErr := writeChunk(map[string]any{"content": content}); writeErr != nil {
					return
				}
			}
			if reasoning := reasoningFilter.Flush(); reasoning != "" {
				if writeErr := writeChunk(map[string]any{"reasoning_content": reasoning}); writeErr != nil {
					return
				}
			}
			res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
			res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
			s.accountPool.MarkSuccess(acc.ID)
		} else {
			log.Printf("[req-trace] id=%s stage=stream_error err=%v", requestID, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
			if convReused {
				s.invalidateConvCache(convCacheScope, acc.ID, convCacheModel)
			}
			msg := upstreamError(err)
			if IsRateLimited(err) {
				msg = "upstream is rate limiting; try again shortly"
			}
			msg = sanitizePublicInternalText(msg)
			trace.fail(upstreamStatus(err), usageErrorType(upstreamStatus(err)), msg)
			stopKeepalive()
			writeOpenAIStreamError(r.Context(), w, flusher, msg, streamErrorCode(err))
			return
		}
		pt := estimatedChatPromptTokens(prompt, &body)
		ct := EstimateTokens(res.Text)
		log.Printf("[usage] stream id=%s pt=%d ct=%d res.Text=%d", id, pt, ct, len(res.Text))
		if ct == 0 {
			stopKeepalive()
			writeOpenAIStreamError(r.Context(), w, flusher, "upstream returned empty completion; the requested model may be unavailable for this tenant", "upstream_error")
			return
		}
		usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
		stopKeepalive()
		_ = emitter.writeRaw("data: " + mustJSON(usageChunk) + "\n\n")
		_ = emitter.writeRaw("data: [DONE]\n\n")
	} else {
		res, err = s.chatWithAccount(ctx, acc.ID, account, answerReq)
		if IsEmptyCompletion(err) && tone != "magic" {
			log.Printf("[tone-fallback] tone=%q returned empty, retrying with magic", tone)
			magicReq := answerReq
			magicReq.Tone = "magic"
			if res2, err2 := s.chatWithAccount(withRetryAttempt(ctx), acc.ID, account, magicReq); err2 == nil && res2.Text != "" {
				res = res2
				err = nil
			}
		}
		if err != nil && body.AccountID == "" && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err)) {
			// Failover only when nothing pins the request to a conversation or
			// account; a fresh chat can safely retry on the next healthy account.
			next, nerr := s.nextHealthyAccount(acc.ID)
			if nerr == nil {
				failoverReq := answerReq
				if body.ConversationID == resolvedConversationID {
					failoverReq.ConversationID = ""
					failoverReq.SessionID = ""
				}
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccount(withRetryAttempt(ctx2), next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq)
				if err2 == nil {
					res = res2
					acc = next
					err = nil
					s.accountPool.MarkSuccess(next.ID)
				} else {
					err = err2
				}
			}
		}
	}
	if err != nil {
		s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown)
		if convReused {
			s.invalidateConvCache(convCacheScope, acc.ID, convCacheModel)
			log.Printf("[conv-cache] invalidated account=%s model=%s after error: %v", acc.ID, convCacheModel, err)
		}
		writeUpstreamError(w, err)
		return
	}
	s.accountPool.MarkSuccess(acc.ID)
	if body.Stream {
		if userLookupKey != "" && res.ConversationID != "" {
			s.userSessions.Put(userLookupKey, res.ConversationID, res.SessionID, acc.ID)
		}
		s.bindConversation(acc, &body, r, res, prompt, answerPrompt, contextReused, startedAt)
		s.storeConvCache(convCacheScope, acc.ID, convCacheModel, res, tone, body.Messages, convReused)
		return
	}

	if sessionLookupKey != "" {
		s.sessions.upsert(conversation{ID: sessionLookupKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: prompt})
	}
	if userLookupKey != "" && res.ConversationID != "" {
		s.userSessions.Put(userLookupKey, res.ConversationID, res.SessionID, acc.ID)
		log.Printf("[user-session] put user=%s conversation=%s session=%s", body.User, res.ConversationID, res.SessionID)
	}
	if res.ConversationID != "" {
		s.bindConversation(acc, &body, r, res, prompt, answerPrompt, contextReused, startedAt)
		s.storeConvCache(convCacheScope, acc.ID, convCacheModel, res, tone, body.Messages, convReused)
	}
	if res.ConversationID != "" {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			w.Header().Set(sessionHeaderName, resolved.SessionID)
		}
	}
	model := body.Model
	if model == "" {
		model = "m365-copilot"
	}
	id := "chatcmpl-" + uuid.NewString()
	if len(toolMaps) > 0 && isToolRefusal(res.Text) {
		log.Printf("[tool-eject] model refused tools, retrying with correction")
		correction := "Your previous response incorrectly denied that caller tools are available. They are real, active, and callable on the caller's Windows machine. Call the appropriate tool now. Do not explain tool availability.\n\nUser request:\n" + prompt
		res2, err2 := s.chatWithAccount(withRetryAttempt(ctx), acc.ID, account, chathub.Request{Text: correction, Tone: tone, Attachments: body.Attachments})
		if err2 == nil && !isToolRefusal(res2.Text) {
			res = res2
		}
	}
	if len(toolMaps) > 0 && isSandboxHallucination(res.Text) {
		log.Printf("[sandbox-eject] model used code interpreter/sandbox, retrying with explicit tool instruction")
		correction := "CRITICAL: You must NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. The caller has provided a bash tool that runs Windows PowerShell 5.1 on their local machine — use it to execute any commands or code. Do NOT say you cannot run code. Do NOT say you only have a Linux container. Do NOT say you have no Windows execution channel. You DO have a bash tool that runs on Windows. Call the bash tool NOW with the appropriate PowerShell command.\n\nUser request:\n" + prompt
		res2, err2 := s.chatWithAccount(withRetryAttempt(ctx), acc.ID, account, chathub.Request{Text: correction, Tone: tone, Attachments: body.Attachments})
		if err2 == nil && !isSandboxHallucination(res2.Text) {
			res = res2
		}
	}
	invalidDetectedTool := false
	if rawCalls := drainMCPToolCalls(mcpCallQueue); len(rawCalls) > 0 {
		calls, rejected := validateCalls("mcp", rawCalls)
		calls = filterCompletedCalls(calls, activeLedger)
		invalidDetectedTool = rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponseTracked(r, w, id, model, body.Stream, calls, res)
			return
		}
	}
	if rawCalls := fencedToolCalls(res.Text, toolMaps, body.ToolChoice); len(rawCalls) > 0 {
		calls, rejected := validateCalls("fenced", rawCalls)
		calls = filterCompletedCalls(calls, activeLedger)
		invalidDetectedTool = rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponseTracked(r, w, id, model, body.Stream, calls, res)
			return
		}
	}
	if rawCalls := nativeToolCalls(res.Events, body.Tools); len(rawCalls) > 0 {
		calls, rejected := validateCalls("native", rawCalls)
		calls = filterCompletedCalls(calls, activeLedger)
		invalidDetectedTool = invalidDetectedTool || rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			_ = writeToolResponseTracked(r, w, id, model, body.Stream, calls, res)
			return
		}
	}
	// Recover natural-language tool intent in native mode, and repair any
	// structured event that failed the declared-name/schema boundary.
	if (planningMode == "native" || invalidDetectedTool) && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routePrompt := modelToolRouterPrompt(prompt+"\n"+activeLedger.RouterContext(), toolMaps, body.ToolChoice)
		routeRes, routeErr := s.chatWithAccount(withToolPlanning(ctx), acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments})
		if routeErr == nil {
			calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
			if !parsed {
				repairRes, repairErr := s.chatWithAccount(withRetryAttempt(withToolPlanning(ctx)), acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments})
				if repairErr == nil {
					calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				}
			}
			calls = filterCompletedCalls(calls, activeLedger)
			calls, _ = validateCalls("native-recovery", calls)
			if parsed && len(calls) > 0 {
				scope := fmt.Sprintf("%d:%v:native-recovery", len(body.Messages), completedCallIDs(activeLedger))
				for i := range calls {
					calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
				}
				calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
				_ = writeToolResponseTracked(r, w, id, model, body.Stream, calls, routeRes)
				return
			}
		}
	}
	if isContentPolicyBlock(res.Text) {
		log.Printf("[content-policy] M365 blocked the request, returning 503")
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	if shouldEnforceCompletionEvidence(toolMaps, activeLedger) && !completionEvidenceAllows(res.Text, activeLedger) {
		res.Text = "I cannot confirm completion because no matching tool results were returned. No external action has been verified."
	}
	res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	log.Printf("[debug] res.Text bytes=%d content=%q", len(res.Text), res.Text)
	created := time.Now().Unix()

	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		// one-shot "stream" — emit full content then done
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": res.Text},
			}},
		}
		b, _ := json.Marshal(chunk)
		_ = sseRaw(r.Context(), w, flusher, "data: "+string(b)+"\n\n")
		pt := estimatedChatPromptTokens(prompt, &body)
		ct := EstimateTokens(res.Text)
		usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
		_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
		_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		return
	}

	if responseFormat != nil && (responseFormat.Type == "json_object" || responseFormat.Type == "json_schema") {
		res.Text = normalizeJSONText(res.Text)
	}
	content := any(res.Text)
	if len(res.Images) > 0 {
		parts := []any{map[string]any{"type": "text", "text": res.Text}}
		for _, u := range res.Images {
			du, _ := downloadImageAsDataURIWithToken(u, acc.AccessToken)
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": du}})
		}
		content = parts
	}
	assistant := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if res.Reasoning != "" {
		assistant["reasoning_content"] = res.Reasoning
	}
	// 上游 ChatHub 不返回 token 计数，按请求/回复文本本地估算填充
	// OpenAI 要求的 usage 字段。
	pt := estimatedChatPromptTokens(prompt, &body)
	ct := EstimateTokens(res.Text)
	jsonOut(w, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       assistant,
			"finish_reason": "stop",
		}},
		"m365": compatM365Metadata(res),
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      pt + ct,
		},
	})
}

func (s *Server) writePublicIdentityChatResponse(w http.ResponseWriter, r *http.Request, body *oaiReq, prompt, answer string, startedAt time.Time) {
	model := firstNonEmpty(body.Model, defaultPublicModelName)
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	inputTokens := EstimateTokens(prompt)
	toolTokens := int64(estimateToolTokens(body.Model, body.Tools, body.ToolChoice))
	outputTokens := EstimateTokens(answer)
	promptTokens := inputTokens + toolTokens
	usage := map[string]any{"prompt_tokens": promptTokens, "completion_tokens": outputTokens, "total_tokens": promptTokens + outputTokens}
	recordUsage := func() {
		s.recordUsage(r, auth.AccountToken{}, UsageRecord{
			Time:         time.Now(),
			Model:        model,
			Endpoint:     "/v1/chat/completions",
			Stream:       body.Stream,
			InputTokens:  inputTokens,
			ToolTokens:   toolTokens,
			OutputTokens: outputTokens,
			DurationMs:   time.Since(startedAt).Milliseconds(),
			Status:       http.StatusOK,
		})
	}
	if !body.Stream {
		jsonOut(w, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": answer},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
		if trace := usageTraceFrom(r.Context()); trace != nil {
			trace.markFirstToken()
		}
		recordUsage()
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	delivery := &streamDeliveryState{}
	emitter := newOpenAIStreamEmitter(r, w, flusher, id, model, usageTraceFrom(r.Context()), delivery)
	if err := emitter.writeDelta(map[string]any{"content": answer}); err != nil {
		return
	}
	finish := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage}
	_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(finish)+"\n\n")
	_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
	recordUsage()
}

const defaultPublicModelName = "m365-copilot"

const sessionHeaderName = "X-M365-Session-Id"

// bindConversation 在请求完成后登记会话解析器索引与缓存统计，流式与非流式
// 路径共用。会话为内容键，云端的对话由 auto_cleanup 按 2h 闲置窗口回收，
// 这里不再做"用完即删"，否则复用永远不可能命中。
func (s *Server) bindConversation(acc auth.AccountToken, body *oaiReq, r *http.Request, res chathub.Result, prompt, sentPrompt string, contextReused bool, startedAt time.Time) {
	if trace := usageTraceFrom(r.Context()); trace != nil && !body.Stream {
		trace.markFirstToken()
	}
	if res.ConversationID != "" {
		if isTransientModelRequest(r) {
			s.dropTransientConversation(res.ConversationID)
		} else {
			historyBody := *body
			historyBody.Messages = append(cloneMessages(body.Messages), oaiMsg{
				Role:             "assistant",
				Content:          res.Text,
				ReasoningContent: res.Reasoning,
			})
			s.sessionResolver.Bind(res.SessionID, res.ConversationID, acc.ID, &historyBody, "", r)
			s.conversationManager.Record(res.ConversationID, acc.ID, prompt)
			if s.conversationManager.ShouldCleanup() {
				if cleaned := s.conversationManager.Cleanup(); len(cleaned) > 0 {
					log.Printf("[conversation-manager] auto-cleaned %d conversations", len(cleaned))
				}
			}
		}
	}

	apiKey := extractAPIKey(r)
	historyTokens := int64(0)
	upper := len(body.Messages) - 1
	if upper < 0 {
		upper = 0
	}
	for _, msg := range body.Messages[:upper] {
		historyTokens += EstimateTokens(contentToString(msg.Content))
	}
	inputTokens := EstimateTokens(sentPrompt)
	tokensSaved := int64(0)
	if contextReused {
		tokensSaved = historyTokens
	}
	sessions := s.sessionResolver.ListSessionsForTenant(requestTenantID(r))
	cacheStats.RecordRequest(apiKey, contextReused, inputTokens, tokensSaved, len(sessions))
	s.recordUsage(r, acc, UsageRecord{
		Time:               time.Now(),
		Model:              firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:           "/v1/chat/completions",
		Stream:             body.Stream,
		InputTokens:        inputTokens,
		ToolTokens:         int64(estimateToolTokens(body.Model, body.Tools, body.ToolChoice)),
		OutputTokens:       EstimateTokens(res.Text),
		HistoryTokens:      historyTokens,
		ConversationReused: contextReused,
		DurationMs:         time.Since(startedAt).Milliseconds(),
		Status:             200,
	})
}

func extractAPIKey(r *http.Request) string {
	key := requestAPICredential(r)
	if len(key) > 8 {
		return key[:8] + "..."
	}
	return key
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func estimatedChatPromptTokens(prompt string, body *oaiReq) int64 {
	if body == nil {
		return EstimateTokens(prompt)
	}
	return EstimateTokens(prompt) + int64(estimateToolTokens(body.Model, body.Tools, body.ToolChoice))
}

func extractOIDTID(accessToken string) (oid, tid string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	if v, ok := m["oid"].(string); ok {
		oid = v
	}
	if v, ok := m["tid"].(string); ok {
		tid = v
	}
	return oid, tid
}
