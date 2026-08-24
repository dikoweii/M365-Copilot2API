package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ssoSecretVersion   = 1
	maxSSOCookies      = 128
	maxSSOCookieBytes  = 32 << 10
	ssoFailureCooldown = 15 * time.Minute
)

var (
	ErrSSOKeyUnavailable = errors.New("SSO encryption key unavailable")
	ErrSSONotConfigured  = errors.New("SSO cookies are not configured")
)

type SSOCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
}

type sealedSSOSecret struct {
	Version    int    `json:"version"`
	KeyID      string `json:"keyId"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type SSOState struct {
	Secret        sealedSSOSecret `json:"secret"`
	UpdatedAt     time.Time       `json:"updatedAt"`
	LastSuccessAt time.Time       `json:"lastSuccessAt,omitempty"`
	NextAttemptAt time.Time       `json:"nextAttemptAt,omitempty"`
	LastErrorCode string          `json:"lastErrorCode,omitempty"`
}

type SSOStatus struct {
	Configured    bool      `json:"configured"`
	KeyAvailable  bool      `json:"keyAvailable"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
	LastSuccessAt time.Time `json:"lastSuccessAt,omitempty"`
	NextAttemptAt time.Time `json:"nextAttemptAt,omitempty"`
	LastErrorCode string    `json:"lastErrorCode,omitempty"`
}

func ssoConfigured(state *SSOState) bool {
	return state != nil && state.Secret.Version == ssoSecretVersion && state.Secret.Ciphertext != ""
}

func ssoRetryReady(state *SSOState, now time.Time) bool {
	return state == nil || state.NextAttemptAt.IsZero() || !now.Before(state.NextAttemptAt)
}

func (s *Store) ssoKeyPath() string {
	if configured := strings.TrimSpace(os.Getenv("M365_SSO_KEY_FILE")); configured != "" {
		return configured
	}
	return filepath.Join(filepath.Dir(s.path), "sso-cookie.key")
}

func loadSSOKey(path string, create bool) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%w: key must contain exactly 32 bytes", ErrSSOKeyUnavailable)
		}
		return key, nil
	}
	if !os.IsNotExist(err) || !create {
		return nil, fmt.Errorf("%w: %v", ErrSSOKeyUnavailable, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSSOKeyUnavailable, err)
	}
	key = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return loadSSOKey(path, false)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSSOKeyUnavailable, err)
	}
	if _, err = f.Write(key); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return key, nil
}

func ssoKeyID(key []byte) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:8])
}

func ssoAAD(accountID string) []byte {
	return []byte("m365-copilot2api:sso-cookie:v1:" + accountID)
}

func normalizeSSOCookies(cookies []SSOCookie) ([]SSOCookie, error) {
	if len(cookies) == 0 {
		return nil, errors.New("at least one SSO cookie is required")
	}
	if len(cookies) > maxSSOCookies {
		return nil, fmt.Errorf("too many SSO cookies: limit is %d", maxSSOCookies)
	}
	byKey := make(map[string]SSOCookie, len(cookies))
	for _, cookie := range cookies {
		cookie.Name = strings.TrimSpace(cookie.Name)
		cookie.Domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cookie.Domain)), ".")
		cookie.Path = strings.TrimSpace(cookie.Path)
		if cookie.Path == "" {
			cookie.Path = "/"
		}
		if cookie.Name == "" || cookie.Value == "" {
			return nil, errors.New("SSO cookie name and value are required")
		}
		if cookie.Domain == "" {
			cookie.Domain = "login.microsoftonline.com"
		}
		if cookie.Domain != "login.microsoftonline.com" {
			return nil, fmt.Errorf("unsupported SSO cookie domain %q", cookie.Domain)
		}
		if !strings.HasPrefix(cookie.Path, "/") {
			return nil, errors.New("SSO cookie path must start with /")
		}
		if containsCookieControl(cookie.Name) || containsCookieControl(cookie.Value) || containsCookieControl(cookie.Path) {
			return nil, errors.New("SSO cookie contains a control character")
		}
		cookie.Secure = true
		byKey[cookie.Domain+"\x00"+cookie.Path+"\x00"+cookie.Name] = cookie
	}
	normalized := make([]SSOCookie, 0, len(byKey))
	for _, cookie := range byKey {
		normalized = append(normalized, cookie)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Name != normalized[j].Name {
			return normalized[i].Name < normalized[j].Name
		}
		return normalized[i].Path < normalized[j].Path
	})
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxSSOCookieBytes {
		return nil, fmt.Errorf("SSO cookie payload exceeds %d bytes", maxSSOCookieBytes)
	}
	return normalized, nil
}

func containsCookieControl(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

func sealSSOCookies(key []byte, accountID string, cookies []SSOCookie) (sealedSSOSecret, error) {
	plain, err := json.Marshal(cookies)
	if err != nil {
		return sealedSSOSecret{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return sealedSSOSecret{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return sealedSSOSecret{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return sealedSSOSecret{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, ssoAAD(accountID))
	return sealedSSOSecret{
		Version:    ssoSecretVersion,
		KeyID:      ssoKeyID(key),
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}, nil
}

func openSSOCookies(key []byte, accountID string, secret sealedSSOSecret) ([]SSOCookie, error) {
	if secret.Version != ssoSecretVersion || secret.KeyID != ssoKeyID(key) {
		return nil, fmt.Errorf("%w: key id or secret version mismatch", ErrSSOKeyUnavailable)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(secret.Nonce)
	if err != nil {
		return nil, errors.New("invalid SSO secret nonce")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(secret.Ciphertext)
	if err != nil {
		return nil, errors.New("invalid SSO secret ciphertext")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, ssoAAD(accountID))
	if err != nil {
		return nil, errors.New("SSO cookie decryption failed")
	}
	var cookies []SSOCookie
	if err := json.Unmarshal(plain, &cookies); err != nil {
		return nil, errors.New("invalid decrypted SSO cookie payload")
	}
	return normalizeSSOCookies(cookies)
}

func (s *Store) SaveSSOCookies(id string, cookies []SSOCookie) error {
	s.mu.Lock()
	accountID := ""
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id || s.data.Accounts[i].OID == id || s.data.Accounts[i].Email == id {
			accountID = s.data.Accounts[i].ID
			break
		}
	}
	s.mu.Unlock()
	if accountID == "" {
		return errors.New("account not found")
	}
	normalized, err := normalizeSSOCookies(cookies)
	if err != nil {
		return err
	}
	key, err := loadSSOKey(s.ssoKeyPath(), true)
	if err != nil {
		return err
	}
	secret, err := sealSSOCookies(key, accountID, normalized)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == accountID {
			s.data.Accounts[i].SSO = &SSOState{Secret: secret, UpdatedAt: time.Now()}
			s.data.Accounts[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return errors.New("account not found")
}

func (s *Store) LoadSSOCookies(id string) ([]SSOCookie, error) {
	s.mu.Lock()
	var accountID string
	var state *SSOState
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id || s.data.Accounts[i].OID == id || s.data.Accounts[i].Email == id {
			accountID = s.data.Accounts[i].ID
			if s.data.Accounts[i].SSO != nil {
				copyState := *s.data.Accounts[i].SSO
				state = &copyState
			}
			break
		}
	}
	s.mu.Unlock()
	if accountID == "" {
		return nil, errors.New("account not found")
	}
	if !ssoConfigured(state) {
		return nil, ErrSSONotConfigured
	}
	key, err := loadSSOKey(s.ssoKeyPath(), false)
	if err != nil {
		return nil, err
	}
	return openSSOCookies(key, accountID, state.Secret)
}

func (s *Store) ClearSSOCookies(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id || s.data.Accounts[i].OID == id || s.data.Accounts[i].Email == id {
			s.data.Accounts[i].SSO = nil
			s.data.Accounts[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return errors.New("account not found")
}

func (s *Store) SSOStatus(id string) (SSOStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		account := s.data.Accounts[i]
		if account.ID == id || account.OID == id || account.Email == id {
			status := SSOStatus{Configured: ssoConfigured(account.SSO)}
			if account.SSO != nil {
				status.UpdatedAt = account.SSO.UpdatedAt
				status.LastSuccessAt = account.SSO.LastSuccessAt
				status.NextAttemptAt = account.SSO.NextAttemptAt
				status.LastErrorCode = account.SSO.LastErrorCode
			}
			if status.Configured {
				key, err := loadSSOKey(s.ssoKeyPath(), false)
				status.KeyAvailable = err == nil && account.SSO.Secret.KeyID == ssoKeyID(key)
			}
			return status, nil
		}
	}
	return SSOStatus{}, errors.New("account not found")
}

func (s *Store) updateSSOResult(id string, success bool, code string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID != id {
			continue
		}
		state := s.data.Accounts[i].SSO
		if !ssoConfigured(state) {
			return nil
		}
		if success {
			state.LastSuccessAt = now
			state.NextAttemptAt = time.Time{}
			state.LastErrorCode = ""
		} else {
			state.NextAttemptAt = now.Add(ssoFailureCooldown)
			state.LastErrorCode = code
		}
		s.data.Accounts[i].UpdatedAt = now
		return s.saveLocked()
	}
	return errors.New("account not found")
}

func terminalCredentialError(err error) bool {
	if err == nil {
		return false
	}
	if strings.HasPrefix(err.Error(), "token_expired:") {
		return true
	}
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		return false
	}
	return oauthErr.Code == "invalid_grant" || oauthErr.AADSTS == "AADSTS700084" || oauthErr.AADSTS == "AADSTS700082"
}

func ssoErrorCode(err error) string {
	var oauthErr *OAuthError
	if errors.As(err, &oauthErr) {
		if oauthErr.AADSTS != "" {
			return oauthErr.AADSTS
		}
		if oauthErr.Code != "" {
			return oauthErr.Code
		}
	}
	if errors.Is(err, ErrSSOKeyUnavailable) {
		return "key_unavailable"
	}
	return "sso_reauth_failed"
}
