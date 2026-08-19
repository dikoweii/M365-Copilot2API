package web

import (
	"net/http/httptest"
	"testing"
)

func TestConversationCacheRequiresAndIsolatesScope(t *testing.T) {
	c := newConversationCache()
	c.Store("scope-a", "account", "model", &cachedConversation{ConversationID: "conversation-a"})
	c.Store("scope-b", "account", "model", &cachedConversation{ConversationID: "conversation-b"})

	if got := c.Lookup("", "account", "model"); got != nil {
		t.Fatalf("unscoped lookup returned %#v", got)
	}
	if got := c.Lookup("scope-a", "account", "model"); got == nil || got.ConversationID != "conversation-a" {
		t.Fatalf("scope-a lookup = %#v", got)
	}
	if got := c.Lookup("scope-b", "account", "model"); got == nil || got.ConversationID != "conversation-b" {
		t.Fatalf("scope-b lookup = %#v", got)
	}
}

func TestConversationCacheScopeIncludesTenantAndSession(t *testing.T) {
	reqA := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqA.Header.Set("Authorization", "Bearer tenant-a")
	reqB := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqB.Header.Set("Authorization", "Bearer tenant-b")

	scopeA := conversationCacheScope(reqA, oaiReq{SessionKey: "task-1"})
	scopeAAgain := conversationCacheScope(reqA, oaiReq{SessionKey: "task-1"})
	scopeB := conversationCacheScope(reqB, oaiReq{SessionKey: "task-1"})
	scopeOtherSession := conversationCacheScope(reqA, oaiReq{SessionKey: "task-2"})

	if scopeA == "" || scopeA != scopeAAgain {
		t.Fatalf("scope must be stable and non-empty: %q %q", scopeA, scopeAAgain)
	}
	if scopeA == scopeB {
		t.Fatal("different API tenants shared a conversation cache scope")
	}
	if scopeA == scopeOtherSession {
		t.Fatal("different session ids shared a conversation cache scope")
	}
	if got := conversationCacheScope(reqA, oaiReq{}); got != "" {
		t.Fatalf("request without explicit identity received scope %q", got)
	}
}

func TestConversationCacheScopeUsesFullCredential(t *testing.T) {
	reqA := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqA.Header.Set("Authorization", "Bearer shared-prefix-credential-a")
	reqB := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	reqB.Header.Set("Authorization", "Bearer shared-prefix-credential-b")

	scopeA := conversationCacheScope(reqA, oaiReq{SessionKey: "task-1"})
	scopeB := conversationCacheScope(reqB, oaiReq{SessionKey: "task-1"})
	if scopeA == scopeB {
		t.Fatal("credentials with the same display prefix shared a cache scope")
	}
}

func TestConversationCacheInvalidationIsScoped(t *testing.T) {
	c := newConversationCache()
	c.Store("scope-a", "account", "model", &cachedConversation{ConversationID: "conversation-a"})
	c.Store("scope-b", "account", "model", &cachedConversation{ConversationID: "conversation-b"})
	c.Invalidate("scope-a", "account", "model")

	if got := c.Lookup("scope-a", "account", "model"); got != nil {
		t.Fatalf("invalidated scope still returned %#v", got)
	}
	if got := c.Lookup("scope-b", "account", "model"); got == nil || got.ConversationID != "conversation-b" {
		t.Fatalf("invalidation crossed scopes: %#v", got)
	}
}
