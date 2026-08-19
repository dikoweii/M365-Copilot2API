package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvPasswordOverridesCompromisedPersistedFile(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	if err := os.MkdirAll(filepath.Dir(persisted), 0700); err != nil {
		t.Fatal(err)
	}
	legacyPassword := strings.Join([]string{"admin", "123"}, "")
	if err := os.WriteFile(persisted, []byte(legacyPassword+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_DATA_DIR", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", "custom-password")

	got, mustChange := loadAdminPassword()
	if got != "custom-password" || mustChange {
		t.Fatalf("loadAdminPassword()=(%q,%v)", got, mustChange)
	}
	b, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "custom-password\n" {
		t.Fatalf("env password not persisted: %q", b)
	}
}

func TestCompromisedEnvironmentPasswordIsRejected(t *testing.T) {
	legacyPassword := strings.Join([]string{"admin", "123"}, "")
	t.Setenv("M365_DATA_DIR", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "admin-password"))
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", legacyPassword)

	got, mustChange := loadAdminPassword()
	if got != "" || mustChange {
		t.Fatalf("loadAdminPassword()=(%q,%v)", got, mustChange)
	}
}

func TestBootstrapPasswordUsesWritablePersistentPath(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	bootstrap := filepath.Join(dir, "secret")
	if err := os.WriteFile(bootstrap, []byte("bootstrap-password\n"), 0400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", bootstrap)
	t.Setenv("M365_ADMIN_PASSWORD", "")

	got, mustChange := loadAdminPassword()
	if got != "bootstrap-password" || mustChange {
		t.Fatalf("loadAdminPassword()=(%q,%v)", got, mustChange)
	}
	if err := saveAdminPassword("a-new-password-123"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "a-new-password-123\n" {
		t.Fatalf("persisted password=%q", b)
	}
}
