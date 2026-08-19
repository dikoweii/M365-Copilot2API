package chathub

import (
	"context"
	"net"
	"testing"
)

func TestPinnedRemoteDialerRejectsRedirectHostChange(t *testing.T) {
	dial := pinnedRemoteDialer("images.example.com", []net.IP{net.ParseIP("203.0.113.10")})
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:443"); err == nil {
		t.Fatal("dialer accepted a host different from the validated URL")
	}
}

func TestValidateRemoteDownloadURLRejectsLoopbackRedirectTarget(t *testing.T) {
	if err := validateRemoteDownloadURL("https://127.0.0.1/private.png"); err == nil {
		t.Fatal("loopback redirect target was accepted")
	}
}
