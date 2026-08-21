package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/chathub"
)

const defaultAccountConcurrency = 8

type accountConcurrency struct {
	mu       sync.Mutex
	limit    int
	inflight map[string]int
	changed  chan struct{}
}

func newAccountConcurrency() *accountConcurrency {
	limit := defaultAccountConcurrency
	if raw := strings.TrimSpace(os.Getenv("M365_ACCOUNT_DEFAULT_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return &accountConcurrency{limit: limit, inflight: map[string]int{}, changed: make(chan struct{})}
}

func (c *accountConcurrency) Available(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID] < c.limit
}

func (c *accountConcurrency) Inflight(accountID string) int {
	if c == nil || accountID == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID]
}

func (c *accountConcurrency) Acquire(ctx context.Context, accountID string) (func(), error) {
	if c == nil || accountID == "" {
		return func() {}, nil
	}
	for {
		c.mu.Lock()
		if c.inflight[accountID] < c.limit {
			c.inflight[accountID]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					if c.inflight[accountID] <= 1 {
						delete(c.inflight, accountID)
					} else {
						c.inflight[accountID]--
					}
					close(c.changed)
					c.changed = make(chan struct{})
					c.mu.Unlock()
				})
			}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *accountConcurrency) Snapshot() map[string]any {
	if c == nil {
		return map[string]any{"limit": defaultAccountConcurrency, "inflight": map[string]int{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inflight := make(map[string]int, len(c.inflight))
	for accountID, count := range c.inflight {
		inflight[accountID] = count
	}
	return map[string]any{"limit": c.limit, "inflight": inflight}
}

func (s *Server) accountAvailable(accountID string) bool {
	if s.tokens != nil && !s.tokens.ScheduleEnabled(accountID) {
		return false
	}
	healthy := s.accountPool == nil || s.accountPool.Available(accountID)
	return healthy && s.accountConcurrency.Available(accountID)
}

func (s *Server) accountClient(accountID string) *chathub.Client {
	if acc, ok := s.tokens.Get(accountID); ok && acc.BoundProxy != "" {
		return s.clientForProxy(acc.BoundProxy)
	}
	return s.chat
}

func (s *Server) chatWithAccount(ctx context.Context, accountID string, account chathub.Account, request chathub.Request) (chathub.Result, error) {
	queueStarted := time.Now()
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	trace := usageTraceFrom(ctx)
	trace.setAccount(accountID)
	trace.addQueue(time.Since(queueStarted))
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	var generation uint64
	if s.accountPool != nil {
		generation = s.accountPool.MarkCall(accountID)
	}
	started := time.Now()
	result, err := s.accountClient(accountID).Chat(ctx, account, request)
	observed := time.Since(started)
	if result.FirstDeltaMs > 0 {
		observed = time.Duration(result.FirstDeltaMs) * time.Millisecond
	}
	trace.observeResult(result, time.Since(started), isToolPlanning(ctx), isRetryAttempt(ctx))
	if s.accountPool != nil {
		s.accountPool.Observe(accountID, observed, err)
		s.accountPool.MarkResult(accountID, generation, err, rateLimitCooldown)
	}
	return result, err
}

func (s *Server) chatWithAccountEvents(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
	queueStarted := time.Now()
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	trace := usageTraceFrom(ctx)
	trace.setAccount(accountID)
	trace.addQueue(time.Since(queueStarted))
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	var generation uint64
	if s.accountPool != nil {
		generation = s.accountPool.MarkCall(accountID)
	}
	started := time.Now()
	result, err := s.accountClient(accountID).ChatWithEvents(ctx, account, request, onEvent)
	observed := time.Since(started)
	if result.FirstDeltaMs > 0 {
		observed = time.Duration(result.FirstDeltaMs) * time.Millisecond
	}
	trace.observeResult(result, time.Since(started), isToolPlanning(ctx), isRetryAttempt(ctx))
	if s.accountPool != nil {
		s.accountPool.Observe(accountID, observed, err)
		s.accountPool.MarkResult(accountID, generation, err, rateLimitCooldown)
	}
	return result, err
}

func (s *Server) chatWithAccountReasoning(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onDelta, onReasoning func(string) error) (chathub.Result, error) {
	queueStarted := time.Now()
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	trace := usageTraceFrom(ctx)
	trace.setAccount(accountID)
	trace.addQueue(time.Since(queueStarted))
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	var generation uint64
	if s.accountPool != nil {
		generation = s.accountPool.MarkCall(accountID)
	}
	started := time.Now()
	result, err := s.accountClient(accountID).ChatWithReasoning(ctx, account, request, onDelta, onReasoning)
	observed := time.Since(started)
	if result.FirstDeltaMs > 0 {
		observed = time.Duration(result.FirstDeltaMs) * time.Millisecond
	}
	trace.observeResult(result, time.Since(started), isToolPlanning(ctx), isRetryAttempt(ctx))
	if s.accountPool != nil {
		s.accountPool.Observe(accountID, observed, err)
		s.accountPool.MarkResult(accountID, generation, err, rateLimitCooldown)
	}
	return result, err
}
