package chathub

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// validateRemoteDownloadURL blocks SSRF: only https and public routable
// addresses are accepted, with a lookup-time recheck against private,
// loopback, link-local and cloud metadata ranges.
func validateRemoteDownloadURL(raw string) error {
	_, _, err := resolveRemoteDownloadURL(context.Background(), raw)
	return err
}

func resolveRemoteDownloadURL(ctx context.Context, raw string) (*url.URL, []net.IP, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid attachment URL")
	}
	if !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return nil, nil, fmt.Errorf("attachment download requires an HTTPS URL without user info")
	}
	host := u.Hostname()
	if host == "" {
		return nil, nil, fmt.Errorf("attachment URL has no host")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, nil, fmt.Errorf("attachment host does not resolve")
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		ips = append(ips, address.IP)
	}
	for _, ip := range ips {
		if ipUnsafe(ip) {
			return nil, nil, fmt.Errorf("attachment URL targets a non-public address")
		}
	}
	return u, ips, nil
}

const maxRemoteImageRedirects = 3

func downloadRemoteImage(ctx context.Context, raw string, maxBytes int64) ([]byte, string, error) {
	current := raw
	for redirects := 0; redirects <= maxRemoteImageRedirects; redirects++ {
		u, ips, err := resolveRemoteDownloadURL(ctx, current)
		if err != nil {
			return nil, "", err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = pinnedRemoteDialer(u.Hostname(), ips)
		client := &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			transport.CloseIdleConnections()
			return nil, "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			return nil, "", err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			transport.CloseIdleConnections()
			if redirects == maxRemoteImageRedirects {
				return nil, "", fmt.Errorf("attachment download exceeded %d redirects", maxRemoteImageRedirects)
			}
			next, err := u.Parse(location)
			if err != nil || location == "" {
				return nil, "", fmt.Errorf("attachment redirect has an invalid location")
			}
			current = next.String()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			transport.CloseIdleConnections()
			return nil, "", fmt.Errorf("attachment download returned HTTP %d", resp.StatusCode)
		}
		contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		if !strings.HasPrefix(contentType, "image/") {
			resp.Body.Close()
			transport.CloseIdleConnections()
			return nil, "", fmt.Errorf("attachment response is not an image")
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		resp.Body.Close()
		transport.CloseIdleConnections()
		if err != nil {
			return nil, "", err
		}
		if int64(len(body)) > maxBytes {
			return nil, "", fmt.Errorf("attachment exceeds %d bytes", maxBytes)
		}
		return body, contentType, nil
	}
	return nil, "", fmt.Errorf("attachment redirect loop")
}

func pinnedRemoteDialer(expectedHost string, ips []net.IP) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(expectedHost, ".")) {
			return nil, fmt.Errorf("attachment dial target changed after validation")
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func ipUnsafe(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 link-local is covered above on Go >= 1.17;
		// 100.64.0.0/10 (CGNAT) is not private per IP.IsPrivate.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	// 169.254.169.254 cloud metadata is link-local; belt and braces.
	if strings.HasPrefix(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}
