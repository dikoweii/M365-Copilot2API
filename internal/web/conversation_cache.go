package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/chathub"
)

type cachedConversation struct {
	ConversationID string
	SessionID      string
	Tone           string
	TurnCount      int
	MessageCount   int
	CreatedAt      time.Time
	LastUsedAt     time.Time
	SystemPrompt   string
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*cachedConversation
	maxAge  time.Duration
}

func newConversationCache() *conversationCache {
	return &conversationCache{
		entries: make(map[string]*cachedConversation),
		maxAge:  20 * time.Minute,
	}
}

func (c *conversationCache) key(scope, accountID, model string) string {
	return scope + "|" + accountID + "|" + model
}

func (c *conversationCache) Lookup(scope, accountID, model string) *cachedConversation {
	if strings.TrimSpace(scope) == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.key(scope, accountID, model)
	entry := c.entries[key]
	if entry == nil {
		return nil
	}
	if time.Since(entry.LastUsedAt) > c.maxAge {
		delete(c.entries, key)
		return nil
	}
	return entry
}

func (c *conversationCache) Store(scope, accountID, model string, conv *cachedConversation) {
	if strings.TrimSpace(scope) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv.LastUsedAt = time.Now()
	c.entries[c.key(scope, accountID, model)] = conv
}

func (c *conversationCache) Invalidate(scope, accountID, model string) {
	if strings.TrimSpace(scope) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, c.key(scope, accountID, model))
}

func (c *conversationCache) GC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.entries {
		if now.Sub(v.LastUsedAt) > c.maxAge {
			delete(c.entries, k)
		}
	}
}

func (c *conversationCache) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"cached_conversations": len(c.entries)}
}

func systemPromptHash(messages []oaiMsg) string {
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			text := contentToString(m.Content)
			if len(text) > 500 {
				text = text[:500]
			}
			h := sha256.Sum256([]byte(text))
			return hex.EncodeToString(h[:])
		}
	}
	return ""
}

func extractLastUserMessage(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}

func conversationCacheScope(r *http.Request, body oaiReq) string {
	identity := strings.TrimSpace(body.SessionKey)
	if identity == "" {
		identity = strings.TrimSpace(body.User)
	}
	if identity == "" {
		return ""
	}
	tenant := requestTenantID(r)
	h := sha256.Sum256([]byte(tenant + "\x00" + identity))
	return hex.EncodeToString(h[:])
}

func (s *Server) storeConvCache(scope, accID, model string, res chathub.Result, tone string, messages []oaiMsg, reused bool) {
	if res.ConversationID == "" {
		return
	}
	cached := s.convCache.Lookup(scope, accID, model)
	entry := &cachedConversation{
		ConversationID: res.ConversationID,
		SessionID:      res.SessionID,
		Tone:           tone,
		MessageCount:   len(messages),
		SystemPrompt:   systemPromptHash(messages),
	}
	if cached != nil && cached.ConversationID == res.ConversationID {
		entry.TurnCount = cached.TurnCount + 1
	} else {
		entry.TurnCount = 1
	}
	s.convCache.Store(scope, accID, model, entry)
}

func (s *Server) invalidateConvCache(scope, accID, model string) {
	s.convCache.Invalidate(scope, accID, model)
}
