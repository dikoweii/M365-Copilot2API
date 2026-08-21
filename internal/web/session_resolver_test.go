package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionResolverUsesDataDirByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_SESSION_CACHE", "")

	sr := openSessionResolver()
	if want := filepath.Join(dir, "sessions.json"); sr.path != want {
		t.Fatalf("session cache path = %q, want %q", sr.path, want)
	}
}

func TestResolveContentKeyedSameIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	// 首次请求绑定云端对话，同一 IP/UA 但不同 user 账户。
	sr.Bind("", "conv-shared", "acc1",
		&oaiReq{User: "alice", Messages: []oaiMsg{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "你好"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 续接请求来自同一 IP/UA（换 user 仍可命中，说明不做 user 拦截）。
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "bob"),
		&oaiReq{
			User: "bob",
			Messages: []oaiMsg{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "你好"},
				{Role: "user", Content: "多说点"},
			},
		})
	if res.IsNew {
		t.Fatal("同 IP/UA 前缀相同却未复用会话，内容键失效")
	}
	if res.MatchedBy != "context_prefix_2" {
		t.Fatalf("expected context_prefix_2, got %q", res.MatchedBy)
	}
	if res.ConversationID != "conv-shared" {
		t.Fatalf("expected conversation conv-shared, got %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 (增量起点), got %d", res.HistoryLen)
	}
}

func TestResolveDoesNotMatchAcrossIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-a", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 不同 IP / UA 的用户输入同样的短消息，不应串到别人的会话。
	res := sr.Resolve(resolverTestRequest("198.51.100.99", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("跨 IP/UA 的内容必须新建会话，got matched=%s conv=%s", res.MatchedBy, res.ConversationID)
	}
}

func TestResolveSingleMessageReusesForSameUser(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("sess-short", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if res.IsNew {
		t.Fatalf("same user re-sending a message should reuse session, got IsNew=true")
	}
}

func TestResolveSingleMessageNeverReusesAcrossUsers(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("sess-short", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	res := sr.Resolve(resolverTestRequest("203.0.113.20", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("different user must not reuse session, got matched=%s", res.MatchedBy)
	}
}

func resolverTestRequest(ip, ua, user string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("User-Agent", ua)
	return r
}

func TestResolverIncrementalBoundary(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("", "conv-inc", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 第二轮只应发送历史之外的新增消息。
	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		}})
	if res.IsNew {
		t.Fatal("增量请求应复用以 2 轮历史为前缀的会话")
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2, got %d", res.HistoryLen)
	}

	// 内容不再是前一轮任何历史的前缀时不应误命中。
	res2 := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "全新问题完全无关"}}})
	if !res2.IsNew {
		t.Fatalf("不相关内容必须新建会话, got %s conv=%s", res2.MatchedBy, res2.ConversationID)
	}
}

func TestResolverMatchesLongHistoryTailWindow(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	history := make([]oaiMsg, 190)
	for i := range history {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history[i] = oaiMsg{Role: role, Content: fmt.Sprintf("long-history-%03d", i+1)}
	}
	req := resolverTestRequest("203.0.113.10", "client-long", "alice")
	sr.Bind("sess-long", "conv-long", "acc-long", &oaiReq{Messages: history}, "", req)

	stored, ok := sr.GetSessionForTenant("local", "sess-long")
	if !ok {
		t.Fatal("long session was not stored")
	}
	if got := len(stored.ContextHistory); got != 128 {
		t.Fatalf("stored history length = %d, want 128", got)
	}
	if !messagesEqual(stored.ContextHistory[0], history[62]) {
		t.Fatal("stored history is not the tail of the prior request")
	}

	next := append([]oaiMsg(nil), history...)
	next = append(next, oaiMsg{Role: "user", Content: "read chapter-010.md"})
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-long", "alice"), &oaiReq{Messages: next})
	if res.IsNew || res.ConversationID != "conv-long" || res.AccountID != "acc-long" {
		t.Fatalf("long history did not reuse its bound conversation: %#v", res)
	}
	if res.HistoryLen != 190 {
		t.Fatalf("HistoryLen = %d, want absolute boundary 190", res.HistoryLen)
	}
	if res.MatchedBy != "context_window_190" {
		t.Fatalf("MatchedBy = %q, want context_window_190", res.MatchedBy)
	}
	if got := next[res.HistoryLen:]; len(got) != 1 || contentToString(got[0].Content) != "read chapter-010.md" {
		t.Fatalf("incremental messages = %#v, want only the new user turn", got)
	}

	other := sr.Resolve(resolverTestRequest("198.51.100.20", "client-other", "bob"), &oaiReq{Messages: next})
	if !other.IsNew {
		t.Fatalf("different IP/UA reused long session: %#v", other)
	}
}

func TestResolverExplicitSessionUsesAbsoluteLongHistoryBoundary(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	history := make([]oaiMsg, 190)
	for i := range history {
		history[i] = oaiMsg{Role: "user", Content: fmt.Sprintf("explicit-%03d", i+1)}
	}
	bindReq := resolverTestRequest("203.0.113.10", "client-explicit", "alice")
	bindReq.Header.Set("X-M365-Session-Id", "client-session")
	sr.Bind("upstream-session", "conv-explicit", "acc-explicit", &oaiReq{Messages: history}, "", bindReq)

	next := append([]oaiMsg(nil), history...)
	next = append(next, oaiMsg{Role: "user", Content: "next"})
	resolveReq := resolverTestRequest("203.0.113.10", "client-explicit", "alice")
	resolveReq.Header.Set("X-M365-Session-Id", "client-session")
	res := sr.Resolve(resolveReq, &oaiReq{Messages: next})
	if res.IsNew || res.HistoryLen != 190 {
		t.Fatalf("explicit long-session boundary = %#v, want HistoryLen 190", res)
	}
}

func TestResolverEvictsAfterTTL(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("sess-old", "conv-old", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 把会话标记为超过默认 2h 闲置。
	sr.mu.Lock()
	storageKey := tenantScopedID("local", "sess-old")
	old := sr.sessions[storageKey]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	sr.sessions[storageKey] = old
	sr.mu.Unlock()

	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}})
	if !res.IsNew {
		t.Fatalf("闲置超 TTL 的会话应失效，got matched=%s", res.MatchedBy)
	}
}

func TestResolverIsolatesExplicitSessionAcrossTenants(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "same request"}}}

	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set("Authorization", "Bearer shared-prefix-tenant-a")
	reqA.Header.Set("X-M365-Session-ID", "shared-session")
	sr.Bind("upstream-session-a", "conversation-a", "account-a", body, "", reqA)

	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB.Header.Set("Authorization", "Bearer shared-prefix-tenant-b")
	reqB.Header.Set("X-M365-Session-ID", "shared-session")
	if got := sr.Resolve(reqB, body); !got.IsNew {
		t.Fatalf("tenant B reused tenant A conversation: %#v", got)
	}

	sr.Bind("upstream-session-b", "conversation-b", "account-b", body, "", reqB)
	if got := sr.Resolve(reqA, body); got.IsNew || got.ConversationID != "conversation-a" {
		t.Fatalf("tenant A explicit session = %#v", got)
	}
	if got := sr.Resolve(reqB, body); got.IsNew || got.ConversationID != "conversation-b" {
		t.Fatalf("tenant B explicit session = %#v", got)
	}
}

func TestResolverRejectsLegacyUnscopedPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	t.Setenv("M365_SESSION_CACHE", path)
	legacy := []sessionBinding{{
		SessionID:      "legacy-session",
		ConversationID: "legacy-conversation",
		LastUsedAt:     time.Now().UTC(),
	}}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	sr := openSessionResolver()
	if got := sr.ListSessions(); len(got) != 0 {
		t.Fatalf("loaded legacy unscoped sessions: %#v", got)
	}
}

func TestResolverPersistsHistoryAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", path)

	sr1 := openSessionResolver()
	sr1.Bind("", "conv-persist", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if err := sr1.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重新打开同一缓存文件，历史仍在 → 前缀仍可命中。
	sr2 := openSessionResolver()
	res := sr2.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
			{Role: "user", Content: "follow-up"},
		}})
	if res.IsNew {
		t.Fatal("contextHistory 应持久化，重启后仍可内容复用")
	}
	if res.ConversationID != "conv-persist" {
		t.Fatalf("unexpected conversation %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 after reload, got %d", res.HistoryLen)
	}
}

func TestAutoCleanupDefaultMaxAgeTwoHours(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	s := newTestServerForAutoCleanup(t)
	s.conversationManager.Record("conv-old", "acc1", "old")
	s.conversationManager.mu.Lock()
	old := s.conversationManager.data["conv-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	s.conversationManager.data["conv-old"] = old
	s.conversationManager.mu.Unlock()

	active := s.activeConversationSet(2 * time.Hour)
	if active["conv-old"] {
		t.Error("3h 闲置的会话不应在 2h 保护窗口内")
	}
}
