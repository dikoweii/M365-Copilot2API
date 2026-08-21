package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	ssoLoginHost   = "login.microsoftonline.com"
	ssoRedirectURI = "https://m365.cloud.microsoft/spalanding"
	maxSSOHops     = 12
)

var tenantSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

// ReauthAccountWithCookies silently mints a fresh account token pair using the
// account's Microsoft login cookies. Requests honor the account-bound proxy so
// an account does not unexpectedly change egress IP during reauthentication.
func ReauthAccountWithCookies(acc AccountToken, cookies []SSOCookie) (TokenSet, error) {
	if strings.TrimSpace(acc.TID) == "" || !tenantSegmentPattern.MatchString(acc.TID) {
		return TokenSet{}, errors.New("account tenant id is unavailable for SSO reauthentication")
	}
	clientID := strings.TrimSpace(acc.ClientID)
	if clientID == "" {
		clientID = ClientID()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return reauthWithCookies(ctx, acc, clientID, normalizedOfflineScope(Scope()), cookies)
}

func reauthWithCookies(ctx context.Context, acc AccountToken, clientID, scope string, cookies []SSOCookie) (TokenSet, error) {
	normalized, err := normalizeSSOCookies(cookies)
	if err != nil {
		return TokenSet{}, err
	}
	client, err := ssoHTTPClient(acc.BoundProxy)
	if err != nil {
		return TokenSet{}, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return TokenSet{}, err
	}
	client.Jar = jar
	loginURL := &url.URL{Scheme: "https", Host: ssoLoginHost, Path: "/"}
	httpCookies := make([]*http.Cookie, 0, len(normalized))
	for _, cookie := range normalized {
		httpCookies = append(httpCookies, &http.Cookie{
			Name: cookie.Name, Value: cookie.Value, Domain: ssoLoginHost,
			Path: cookie.Path, Secure: true, HttpOnly: cookie.HTTPOnly,
		})
	}
	jar.SetCookies(loginURL, httpCookies)

	verifier, err := Verifier()
	if err != nil {
		return TokenSet{}, err
	}
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return TokenSet{}, err
	}
	state := hex.EncodeToString(stateBytes)
	authorizeURL := &url.URL{
		Scheme: "https",
		Host:   ssoLoginHost,
		Path:   "/" + acc.TID + "/oauth2/v2.0/authorize",
	}
	query := authorizeURL.Query()
	query.Set("client_id", clientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", ssoRedirectURI)
	query.Set("scope", scope)
	query.Set("response_mode", "fragment")
	query.Set("code_challenge", Challenge(verifier))
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	query.Set("sso_reload", "True")
	authorizeURL.RawQuery = query.Encode()

	current := authorizeURL
	for hop := 0; hop < maxSSOHops; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return TokenSet{}, err
		}
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Referer", "https://m365.cloud.microsoft/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/125 Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			return TokenSet{}, fmt.Errorf("SSO authorize request failed: %w", err)
		}
		location := strings.TrimSpace(resp.Header.Get("Location"))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if location == "" {
			return TokenSet{}, fmt.Errorf("SSO authorize stopped without a redirect (HTTP %d)", resp.StatusCode)
		}
		next, err := current.Parse(location)
		if err != nil {
			return TokenSet{}, errors.New("SSO authorize returned an invalid redirect")
		}
		if next.Scheme != "https" {
			return TokenSet{}, errors.New("SSO authorize returned a non-HTTPS redirect")
		}
		if code, returnedState, oauthErr := ssoRedirectResult(next); code != "" || oauthErr != "" {
			if !strings.EqualFold(next.Hostname(), "m365.cloud.microsoft") {
				return TokenSet{}, errors.New("SSO authorization result came from an unexpected host")
			}
			if returnedState != state {
				return TokenSet{}, errors.New("SSO authorization state mismatch")
			}
			if oauthErr != "" {
				return TokenSet{}, &OAuthError{Code: oauthErr, HTTPStatus: http.StatusUnauthorized}
			}
			return exchangeSSOCode(ctx, client, acc.TID, clientID, scope, code, verifier)
		}
		if !strings.EqualFold(next.Hostname(), ssoLoginHost) {
			return TokenSet{}, fmt.Errorf("SSO authorize redirected to unsupported host %q", next.Hostname())
		}
		current = next
	}
	return TokenSet{}, errors.New("SSO authorize exceeded redirect limit")
}

func ssoHTTPClient(boundProxy string) (*http.Client, error) {
	var base *http.Client
	if strings.TrimSpace(boundProxy) != "" {
		clients, err := outbound.New(boundProxy)
		if err != nil {
			return nil, fmt.Errorf("configure account proxy for SSO: %w", err)
		}
		base = clients.HTTP
	} else {
		base = outbound.HTTPClient()
	}
	client := *base
	client.Timeout = 20 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client, nil
}

func ssoRedirectResult(target *url.URL) (code, state, oauthErr string) {
	values := target.Query()
	if target.Fragment != "" {
		if fragmentValues, err := url.ParseQuery(target.Fragment); err == nil {
			for key, items := range fragmentValues {
				if len(values[key]) == 0 {
					values[key] = items
				}
			}
		}
	}
	return values.Get("code"), values.Get("state"), values.Get("error")
}

func exchangeSSOCode(ctx context.Context, client *http.Client, tenant, clientID, scope, code, verifier string) (TokenSet, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", ssoRedirectURI)
	form.Set("code_verifier", verifier)
	form.Set("scope", scope)
	tokenURL := "https://" + ssoLoginHost + "/" + tenant + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenClient := *client
	tokenClient.Jar = nil
	resp, err := tokenClient.Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("SSO token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenSet{}, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return TokenSet{}, errors.New("SSO token endpoint returned invalid JSON")
	}
	if tr.Error != "" {
		return TokenSet{}, &OAuthError{
			Code: tr.Error, AADSTS: aadstsCode(tr.ErrorDesc), HTTPStatus: resp.StatusCode,
			CorrelationID: firstNonEmpty(tr.CorrelationID, resp.Header.Get("client-request-id")),
			TraceID:       firstNonEmpty(tr.TraceID, resp.Header.Get("x-ms-request-id")),
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.AccessToken == "" || tr.RefreshToken == "" {
		return TokenSet{}, fmt.Errorf("SSO token exchange HTTP %d returned incomplete credentials", resp.StatusCode)
	}
	set := TokenSet{
		AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, IDToken: tr.IDToken,
		TokenType: tr.TokenType, Scope: tr.Scope, ExpiresIn: tr.ExpiresIn,
		ExpiresAt: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	if claims, err := decodeJWTClaims(tr.AccessToken); err == nil {
		set.Email = firstNonEmpty(claims["unique_name"], claims["upn"], claims["preferred_username"], claims["email"])
		set.DisplayName = firstNonEmpty(claims["name"], set.Email)
		set.HomeOID = firstNonEmpty(claims["oid"], claims["sub"])
		set.TenantID = firstNonEmpty(claims["tid"], claims["tenant_id"])
	}
	return set, nil
}

func normalizedOfflineScope(scope string) string {
	seen := map[string]bool{}
	fields := make([]string, 0)
	for _, field := range strings.Fields(scope) {
		if !seen[field] {
			seen[field] = true
			fields = append(fields, field)
		}
	}
	if !seen["offline_access"] {
		fields = append(fields, "offline_access")
	}
	return strings.Join(fields, " ")
}
