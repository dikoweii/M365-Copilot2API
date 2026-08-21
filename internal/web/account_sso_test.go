package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func newAccountSSOTestServer(t *testing.T) (*Server, auth.AccountToken) {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(auth.TokenSet{
		AccessToken: "access", RefreshToken: "refresh", Email: "user@example.test",
		HomeOID: "oid-test", TenantID: "tenant-test", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{tokens: store, accountPool: newAccountHealth()}, acc
}

func TestAccountSSOCookiePutAndDeleteAreSanitized(t *testing.T) {
	s, acc := newAccountSSOTestServer(t)
	payload, _ := json.Marshal(map[string]any{
		"id": acc.ID,
		"cookies": []map[string]any{{
			"name": "ESTSAUTH", "value": "super-secret-cookie", "domain": "login.microsoftonline.com", "path": "/",
		}},
	})
	put := httptest.NewRequest(http.MethodPut, "/api/accounts/sso-cookie", bytes.NewReader(payload))
	putRecorder := httptest.NewRecorder()
	s.accountSSOCookie(putRecorder, put)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRecorder.Code, putRecorder.Body.String())
	}
	if putRecorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing no-store header: %q", putRecorder.Header().Get("Cache-Control"))
	}
	for _, forbidden := range []string{"super-secret-cookie", "ciphertext", "nonce", "keyId"} {
		if strings.Contains(putRecorder.Body.String(), forbidden) {
			t.Fatalf("PUT response leaked %q: %s", forbidden, putRecorder.Body.String())
		}
	}

	accountsRecorder := httptest.NewRecorder()
	s.accounts(accountsRecorder, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if accountsRecorder.Code != http.StatusOK || !strings.Contains(accountsRecorder.Body.String(), `"configured":true`) {
		t.Fatalf("accounts response missing SSO status: %s", accountsRecorder.Body.String())
	}
	for _, forbidden := range []string{"super-secret-cookie", "ciphertext", "nonce", "keyId"} {
		if strings.Contains(accountsRecorder.Body.String(), forbidden) {
			t.Fatalf("accounts response leaked %q: %s", forbidden, accountsRecorder.Body.String())
		}
	}

	deleteRecorder := httptest.NewRecorder()
	s.accountSSOCookie(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/api/accounts/sso-cookie?id="+acc.ID, nil))
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	if _, err := s.tokens.LoadSSOCookies(acc.ID); !errors.Is(err, auth.ErrSSONotConfigured) {
		t.Fatalf("cookies still configured after DELETE: %v", err)
	}
}

func TestAccountSSOCookieRequestLimitAndAdminProtection(t *testing.T) {
	s, _ := newAccountSSOTestServer(t)
	large := `{"id":"x","cookies":[{"name":"ESTSAUTH","value":"` + strings.Repeat("x", maxSSOCookieRequestBytes) + `"}]}`
	recorder := httptest.NewRecorder()
	s.accountSSOCookie(recorder, httptest.NewRequest(http.MethodPut, "/api/accounts/sso-cookie", strings.NewReader(large)))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large request status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/sso-cookie", s.accountSSOCookie)
	s.adminPassword = "configured-password"
	s.adminSessions = map[string]time.Time{}
	protected := httptest.NewRecorder()
	s.adminMiddleware(mux).ServeHTTP(protected, httptest.NewRequest(http.MethodDelete, "/api/accounts/sso-cookie?id=x", nil))
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected SSO endpoint status=%d", protected.Code)
	}
}
