package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSSOTestStore(t *testing.T, refreshToken string) (*Store, AccountToken) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(TokenSet{
		AccessToken: "expired-access", RefreshToken: refreshToken,
		Email: "user@example.test", HomeOID: "oid-test", TenantID: "tenant-test",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSSOCookies(acc.ID, []SSOCookie{{
		Name: "ESTSAUTH", Value: "test-cookie-secret", Domain: "login.microsoftonline.com", Path: "/",
	}}); err != nil {
		t.Fatal(err)
	}
	return store, acc
}

func successfulTestToken(acc AccountToken) TokenSet {
	return TokenSet{
		AccessToken: "new-access", RefreshToken: "new-refresh",
		Email: acc.Email, DisplayName: acc.DisplayName, HomeOID: acc.OID, TenantID: acc.TID,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestSSOCookiesEncryptedRoundTrip(t *testing.T) {
	store, acc := newSSOTestStore(t, "refresh")
	cookies, err := store.LoadSSOCookies(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0].Value != "test-cookie-secret" || !cookies[0].Secure {
		t.Fatalf("unexpected cookies: %#v", cookies)
	}
	persisted, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte("test-cookie-secret")) {
		t.Fatal("accounts.json contains the plaintext SSO cookie")
	}
}

func TestSSOSecretRejectsTamperingAndWrongAccount(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	cookies := []SSOCookie{{Name: "ESTSAUTH", Value: "secret", Domain: "login.microsoftonline.com", Path: "/"}}
	secret, err := sealSSOCookies(key, "account-a", cookies)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSSOCookies(key, "account-b", secret); err == nil {
		t.Fatal("secret decrypted for the wrong account")
	}
	secret.Ciphertext = secret.Ciphertext[:len(secret.Ciphertext)-1] + "A"
	if _, err := openSSOCookies(key, "account-a", secret); err == nil {
		t.Fatal("tampered secret decrypted successfully")
	}
}

func TestSSOCookiesRejectWrongKey(t *testing.T) {
	store, acc := newSSOTestStore(t, "refresh")
	if err := os.WriteFile(store.ssoKeyPath(), bytes.Repeat([]byte{0x24}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadSSOCookies(acc.ID); !errors.Is(err, ErrSSOKeyUnavailable) {
		t.Fatalf("expected ErrSSOKeyUnavailable, got %v", err)
	}
}

func TestOpenStoreAcceptsLegacyAccountJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	legacy := map[string]any{"accounts": []map[string]any{{
		"id": "legacy-id", "email": "legacy@example.test", "accessToken": "a",
		"refreshToken": "r", "expiresAt": time.Now().Add(time.Hour), "status": "active",
	}}}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := store.Get("legacy-id")
	if !ok || acc.SSO != nil {
		t.Fatalf("legacy account was not loaded safely: %#v", acc)
	}
}

func TestRefreshSuccessDoesNotInvokeSSO(t *testing.T) {
	store, acc := newSSOTestStore(t, "refresh")
	store.refresh = func(string) (TokenSet, error) { return successfulTestToken(acc), nil }
	var ssoCalls atomic.Int32
	store.SetSSOReauth(func(AccountToken) (TokenSet, error) {
		ssoCalls.Add(1)
		return TokenSet{}, errors.New("unexpected SSO call")
	})
	if _, err := store.EnsureValid(acc.ID); err != nil {
		t.Fatal(err)
	}
	if ssoCalls.Load() != 0 {
		t.Fatalf("SSO called %d times after successful refresh", ssoCalls.Load())
	}
}

func TestInvalidGrantInvokesSSOOnceForConcurrentRefresh(t *testing.T) {
	store, acc := newSSOTestStore(t, "refresh")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var refreshCalls atomic.Int32
	store.refresh = func(string) (TokenSet, error) {
		refreshCalls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return TokenSet{}, &OAuthError{Code: "invalid_grant", AADSTS: "AADSTS700084", HTTPStatus: 400}
	}
	var ssoCalls atomic.Int32
	store.SetSSOReauth(func(got AccountToken) (TokenSet, error) {
		ssoCalls.Add(1)
		return successfulTestToken(got), nil
	})

	const callers = 8
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := store.EnsureValid(acc.ID)
			errs <- err
		}()
	}
	<-started
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 || ssoCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d SSO calls=%d, want 1 each", refreshCalls.Load(), ssoCalls.Load())
	}
}

func TestTransientRefreshFailureDoesNotInvokeSSO(t *testing.T) {
	store, acc := newSSOTestStore(t, "refresh")
	store.refresh = func(string) (TokenSet, error) { return TokenSet{}, errors.New("connection reset") }
	var ssoCalls atomic.Int32
	store.SetSSOReauth(func(AccountToken) (TokenSet, error) {
		ssoCalls.Add(1)
		return TokenSet{}, nil
	})
	if _, err := store.EnsureValid(acc.ID); err == nil {
		t.Fatal("expected refresh failure")
	}
	if ssoCalls.Load() != 0 {
		t.Fatalf("SSO called %d times for a transient failure", ssoCalls.Load())
	}
}

func TestSSOFailureCooldownPersists(t *testing.T) {
	store, acc := newSSOTestStore(t, "refresh")
	store.refresh = func(string) (TokenSet, error) {
		return TokenSet{}, &OAuthError{Code: "invalid_grant", AADSTS: "AADSTS700082", HTTPStatus: 400}
	}
	var ssoCalls atomic.Int32
	store.SetSSOReauth(func(AccountToken) (TokenSet, error) {
		ssoCalls.Add(1)
		return TokenSet{}, errors.New("cookie rejected")
	})
	if _, err := store.EnsureValid(acc.ID); err == nil {
		t.Fatal("expected SSO failure")
	}
	status, err := store.SSOStatus(acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.NextAttemptAt.IsZero() || status.LastErrorCode == "" {
		t.Fatalf("SSO failure cooldown was not persisted: %#v", status)
	}
	if _, err := store.EnsureValid(acc.ID); err == nil {
		t.Fatal("expected refresh failure during cooldown")
	}
	if ssoCalls.Load() != 1 {
		t.Fatalf("SSO called %d times during cooldown", ssoCalls.Load())
	}
	persisted, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "test-cookie-secret") {
		t.Fatal("plaintext cookie leaked while persisting cooldown")
	}
}
