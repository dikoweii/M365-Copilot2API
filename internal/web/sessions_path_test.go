package web

import (
	"path/filepath"
	"testing"
)

func TestSessionStoresUseDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_SESSION_CACHE", sessionPath)
	t.Setenv("M365_CONVERSATION_INDEX", "")

	legacy := openSessionStore()
	resolver := openSessionResolver()
	if legacy.path == resolver.path {
		t.Fatalf("conversation index and session resolver share %q", legacy.path)
	}
	if got, want := legacy.path, filepath.Join(dir, "conversation-index.json"); got != want {
		t.Fatalf("conversation index path = %q, want %q", got, want)
	}
}
