package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"m365-copilot2api/internal/mcp"
)

func TestAPIKeyAuthenticateReturnsStableRecordID(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, raw, err := store.create("tenant")
	if err != nil {
		t.Fatal(err)
	}
	id, ok := store.authenticate(raw)
	if !ok || id != record.ID {
		t.Fatalf("authenticate = %q, %t; want %q, true", id, ok, record.ID)
	}
}

func TestAPIMiddlewareInjectsVerifiedTenantRecordID(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, raw, err := store.create("tenant")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{apiKeys: store}
	seenTenant := ""
	handler := server.adminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTenant = requestTenantID(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("middleware status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if want := "key:" + record.ID; seenTenant != want {
		t.Fatalf("tenant context = %q, want %q", seenTenant, want)
	}
}

func TestAPIMiddlewareAllowsOnlyLiveMCPCapabilitiesWithoutKey(t *testing.T) {
	mcp.GlobalToolRegistry.ClearTools()
	mcp.GlobalToolRegistry.RegisterToolsForScope("live-scope", []mcp.Tool{{Name: "workspace_read"}})
	t.Cleanup(mcp.GlobalToolRegistry.ClearTools)

	server := &Server{}
	handler := server.adminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	live := httptest.NewRequest(http.MethodGet, "/v1/mcp/sse?scope=live-scope", nil)
	liveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(liveRecorder, live)
	if liveRecorder.Code != http.StatusNoContent {
		t.Fatalf("live capability status = %d", liveRecorder.Code)
	}

	unknown := httptest.NewRequest(http.MethodGet, "/v1/mcp/sse?scope=unknown", nil)
	unknownRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unknownRecorder, unknown)
	if unknownRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unknown capability status = %d, want %d", unknownRecorder.Code, http.StatusUnauthorized)
	}
}

func TestAPIKeyCreateRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir())
	if _, _, err := store.create("test"); err == nil {
		t.Fatal("expected persistence error")
	}
	if got := len(store.Keys); got != 0 {
		t.Fatalf("retained %d in-memory keys after failed save", got)
	}
}

func TestAPIKeyRevokeRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, _, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	store.Path = t.TempDir()
	revoked, err := store.revoke(record.ID)
	if err == nil || revoked {
		t.Fatalf("revoke=%v err=%v, want persistence failure", revoked, err)
	}
	if store.Keys[0].Revoked {
		t.Fatal("key remained revoked after failed save")
	}
}

func TestAPIKeyDeletePhysicallyRemoves(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	r1, _, err := store.create("one")
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := store.create("two")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.delete(r1.ID)
	if err != nil || !deleted {
		t.Fatalf("delete=%v err=%v", deleted, err)
	}
	for _, k := range store.Keys {
		if k.ID == r1.ID {
			t.Fatal("key still present after delete")
		}
	}
	if len(store.Keys) != 1 || store.Keys[0].ID != r2.ID {
		t.Fatalf("unexpected remaining keys: %+v", store.Keys)
	}
	if deleted, _ := store.delete("no-such-id"); deleted {
		t.Fatal("delete of unknown id should report false")
	}
}

func TestAPIKeyDeleteRollsBackWhenPersistenceFails(t *testing.T) {
	store := newAPIKeyStore(t.TempDir() + "/api-keys.json")
	record, _, err := store.create("test")
	if err != nil {
		t.Fatal(err)
	}
	store.Path = t.TempDir()
	deleted, err := store.delete(record.ID)
	if err == nil || deleted {
		t.Fatalf("delete=%v err=%v, want persistence failure", deleted, err)
	}
	if len(store.Keys) != 1 || store.Keys[0].ID != record.ID {
		t.Fatalf("key not restored after failed delete: %+v", store.Keys)
	}
}
