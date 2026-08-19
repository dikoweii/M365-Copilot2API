package web

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type UsageRecord struct {
	Time                 time.Time `json:"time"`
	RequestID            string    `json:"request_id,omitempty"`
	APIKeyPrefix         string    `json:"api_key_prefix"`
	AccountEmail         string    `json:"account_email"`
	ClientIP             string    `json:"client_ip,omitempty"`
	CFRay                string    `json:"cf_ray,omitempty"`
	ClientCountry        string    `json:"client_country,omitempty"`
	Proxy                string    `json:"proxy,omitempty"`
	Model                string    `json:"model"`
	Endpoint             string    `json:"endpoint"`
	Stream               bool      `json:"stream"`
	InputTokens          int64     `json:"input_tokens"`
	ToolTokens           int64     `json:"tool_tokens,omitempty"`
	OutputTokens         int64     `json:"output_tokens"`
	CacheTokens          int64     `json:"cache_tokens"`
	HistoryTokens        int64     `json:"history_tokens,omitempty"`
	ConversationReused   bool      `json:"conversation_reused,omitempty"`
	DurationMs           int64     `json:"duration_ms"`
	TTFTMs               int64     `json:"ttft_ms,omitempty"`
	QueueMs              int64     `json:"queue_ms,omitempty"`
	ConnectMs            int64     `json:"connect_ms,omitempty"`
	ToolPlanningMs       int64     `json:"tool_planning_ms,omitempty"`
	RetryCount           int       `json:"retry_count,omitempty"`
	ToolCallCount        int       `json:"tool_call_count,omitempty"`
	Partial              bool      `json:"partial,omitempty"`
	StreamRecovered      bool      `json:"stream_recovered,omitempty"`
	UpstreamRequestID    string    `json:"upstream_request_id,omitempty"`
	InterruptedRequestID string    `json:"interrupted_request_id,omitempty"`
	LastDeltaMs          int64     `json:"last_delta_ms,omitempty"`
	FailureStage         string    `json:"failure_stage,omitempty"`
	UpstreamCloseCode    int       `json:"upstream_close_code,omitempty"`
	Status               int       `json:"status"`
	ErrorType            string    `json:"error_type,omitempty"`
	ErrorMessage         string    `json:"error_message,omitempty"`
	UsageSource          string    `json:"usage_source,omitempty"`
}

const maxUsageRecords = 50000

type usageLog struct {
	mu      sync.Mutex
	Path    string
	records []UsageRecord
	pending []UsageRecord
	persist *persistStore
}

var globalUsage = &usageLog{}

func openUsageLog() *usageLog {
	p := strings.TrimSpace(os.Getenv("M365_USAGE_LOG"))
	if p == "" {
		dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
		if dir == "" {
			h, _ := os.UserHomeDir()
			dir = filepath.Join(h, ".config", "m365-copilot2api")
		}
		p = filepath.Join(dir, "usage.jsonl")
	}
	s := &usageLog{Path: p}
	s.persist = &persistStore{flush: s.flush}
	_ = os.MkdirAll(filepath.Dir(p), 0700)
	s.load()
	return s
}

func (s *usageLog) load() {
	f, err := os.Open(s.Path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var rec UsageRecord
		if json.Unmarshal(scanner.Bytes(), &rec) == nil {
			s.records = append(s.records, rec)
		}
	}
	s.trim()
}

func (s *usageLog) trim() {
	if len(s.records) > maxUsageRecords {
		s.records = s.records[len(s.records)-maxUsageRecords:]
	}
}

func (s *usageLog) record(rec UsageRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.trim()
	s.pending = append(s.pending, rec)
	s.mu.Unlock()
	s.persist.markDirty()
}

// flush 批量追加本次累积的记录，锁外写盘。
func (s *usageLog) flush() error {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	var buf []byte
	for _, rec := range pending {
		if b, err := json.Marshal(rec); err == nil {
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		s.mu.Lock()
		s.pending = append(pending, s.pending...)
		s.mu.Unlock()
		return err
	}
	defer f.Close()
	_, err = f.Write(buf)
	if err != nil {
		s.mu.Lock()
		s.pending = append(pending, s.pending...)
		s.mu.Unlock()
		return err
	}
	return f.Sync()
}

func (s *usageLog) snapshot(days int) map[string]any {
	s.mu.Lock()
	recs := append([]UsageRecord(nil), s.records...)
	s.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -days)
	now := time.Now()
	loc := now.Location()
	todayLocal := now.In(loc)
	today := time.Date(todayLocal.Year(), todayLocal.Month(), todayLocal.Day(), 0, 0, 0, 0, loc)
	dayAgo := now.Add(-24 * time.Hour)

	var (
		requests, in, tools, out, cache, history, reused, durationMs int64
		successes, errors, ttftMs, ttftSamples                       int64
		todayReq, todayTok                                           int64
		h24Req, h24Tok                                               int64
	)
	durations := make([]int64, 0, len(recs))
	ttfts := make([]int64, 0, len(recs))
	keyCounts := map[string]*usageCountStat{}
	modelCounts := map[string]*usageCountStat{}
	endpointCounts := map[string]*usageCountStat{}
	ipCounts := map[string]*usageCountStat{}
	accountCounts := map[string]*usageCountStat{}
	proxyCounts := map[string]*usageCountStat{}
	statusCounts := map[int]int64{}
	trendMap := map[string]*usageTrendPoint{}

	for _, rec := range recs {
		if rec.Time.Before(cutoff) {
			continue
		}
		requests++
		reqTok := rec.InputTokens + rec.ToolTokens + rec.OutputTokens + rec.CacheTokens
		in += rec.InputTokens
		tools += rec.ToolTokens
		out += rec.OutputTokens
		cache += rec.CacheTokens
		history += rec.HistoryTokens
		if rec.ConversationReused {
			reused++
		}
		durationMs += rec.DurationMs
		durations = append(durations, rec.DurationMs)
		if rec.TTFTMs > 0 {
			ttftMs += rec.TTFTMs
			ttftSamples++
			ttfts = append(ttfts, rec.TTFTMs)
		}
		if rec.Status >= httpStatusSuccessMin && rec.Status < httpStatusErrorMin {
			successes++
		} else {
			errors++
		}
		statusCounts[rec.Status]++
		if rec.Time.After(today) {
			todayReq++
			todayTok += reqTok
		}
		if rec.Time.After(dayAgo) {
			h24Req++
			h24Tok += reqTok
		}
		key := rec.APIKeyPrefix
		ks, ok := keyCounts[key]
		if !ok {
			ks = &usageCountStat{}
			keyCounts[key] = ks
		}
		ks.Requests++
		ks.Tokens += reqTok
		if mc, ok := modelCounts[rec.Model]; ok {
			mc.Requests++
			mc.Tokens += reqTok
		} else {
			modelCounts[rec.Model] = &usageCountStat{Requests: 1, Tokens: reqTok}
		}
		if ec, ok := endpointCounts[rec.Endpoint]; ok {
			ec.Requests++
			ec.Tokens += reqTok
		} else {
			endpointCounts[rec.Endpoint] = &usageCountStat{Requests: 1, Tokens: reqTok}
		}
		addUsageCount(ipCounts, rec.ClientIP, reqTok)
		addUsageCount(accountCounts, rec.AccountEmail, reqTok)
		addUsageCount(proxyCounts, rec.Proxy, reqTok)
		date := rec.Time.In(loc).Format("01-02")
		if tp, ok := trendMap[date]; ok {
			tp.Requests++
			tp.Tokens += reqTok
		} else {
			trendMap[date] = &usageTrendPoint{Date: date, Requests: 1, Tokens: reqTok}
		}
	}

	avgMs := int64(0)
	if requests > 0 {
		avgMs = durationMs / requests
	}
	successRate := float64(0)
	if requests > 0 {
		successRate = float64(successes) * 100 / float64(requests)
	}
	ttftAvgMs := int64(0)
	if ttftSamples > 0 {
		ttftAvgMs = ttftMs / ttftSamples
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })

	model := make([]map[string]any, 0, len(modelCounts))
	for name, c := range modelCounts {
		model = append(model, map[string]any{"name": name, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(model, func(i, j int) bool { return model[i]["tokens"].(int64) > model[j]["tokens"].(int64) })

	ep := make([]map[string]any, 0, len(endpointCounts))
	for k, c := range endpointCounts {
		ep = append(ep, map[string]any{"endpoint": k, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(ep, func(i, j int) bool { return ep[i]["tokens"].(int64) > ep[j]["tokens"].(int64) })

	keys := make([]map[string]any, 0, len(keyCounts))
	for k, c := range keyCounts {
		keys = append(keys, map[string]any{"api_key_prefix": k, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i]["requests"].(int64) > keys[j]["requests"].(int64) })

	statuses := make([]map[string]any, 0, len(statusCounts))
	for status, count := range statusCounts {
		statuses = append(statuses, map[string]any{"status": status, "requests": count})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i]["status"].(int) < statuses[j]["status"].(int) })

	ips := usageDimension(ipCounts, "client_ip")
	accounts := usageDimension(accountCounts, "account_email")
	proxies := usageDimension(proxyCounts, "proxy")

	trend := make([]map[string]any, 0, len(trendMap))
	for _, t := range trendMap {
		trend = append(trend, map[string]any{"date": t.Date, "requests": t.Requests, "tokens": t.Tokens})
	}
	sort.Slice(trend, func(i, j int) bool { return trend[i]["date"].(string) < trend[j]["date"].(string) })

	return map[string]any{
		"summary": map[string]any{
			"requests":            requests,
			"successes":           successes,
			"errors":              errors,
			"success_rate":        successRate,
			"tokens":              in + tools + out + cache,
			"input":               in,
			"tools":               tools,
			"output":              out,
			"cache":               cache,
			"history":             history,
			"conversation_reused": reused,
			"avg_ms":              avgMs,
			"p50_ms":              nearestRankPercentile(durations, 50),
			"p95_ms":              nearestRankPercentile(durations, 95),
			"p99_ms":              nearestRankPercentile(durations, 99),
			"ttft_avg_ms":         ttftAvgMs,
			"ttft_p50_ms":         nearestRankPercentile(ttfts, 50),
			"ttft_p95_ms":         nearestRankPercentile(ttfts, 95),
			"today_requests":      todayReq,
			"today_tokens":        todayTok,
			"last24h_requests":    h24Req,
			"last24h_tokens":      h24Tok,
		},
		"models":    model,
		"endpoints": ep,
		"keys":      keys,
		"statuses":  statuses,
		"ips":       ips,
		"accounts":  accounts,
		"proxies":   proxies,
		"trend":     trend,
	}
}

const (
	httpStatusSuccessMin = 200
	httpStatusErrorMin   = 400
)

func addUsageCount(counts map[string]*usageCountStat, rawKey string, tokens int64) {
	key := strings.TrimSpace(rawKey)
	if key == "" {
		return
	}
	if count, ok := counts[key]; ok {
		count.Requests++
		count.Tokens += tokens
		return
	}
	counts[key] = &usageCountStat{Requests: 1, Tokens: tokens}
}

func usageDimension(counts map[string]*usageCountStat, keyName string) []map[string]any {
	result := make([]map[string]any, 0, len(counts))
	for key, count := range counts {
		result = append(result, map[string]any{
			keyName:    key,
			"requests": count.Requests,
			"tokens":   count.Tokens,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		iRequests := result[i]["requests"].(int64)
		jRequests := result[j]["requests"].(int64)
		if iRequests != jRequests {
			return iRequests > jRequests
		}
		return result[i][keyName].(string) < result[j][keyName].(string)
	})
	return result
}

func nearestRankPercentile(sorted []int64, percentile int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (percentile*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func (s *usageLog) logs(limit, offset int) map[string]any {
	s.mu.Lock()
	recs := append([]UsageRecord(nil), s.records...)
	s.mu.Unlock()

	total := len(recs)
	if offset > total {
		offset = total
	}
	start := total - offset - limit
	if start < 0 {
		start = 0
	}
	end := total - offset
	if end < 0 {
		end = 0
	}
	if start >= end {
		return map[string]any{"logs": []UsageRecord{}, "total": total}
	}
	out := make([]UsageRecord, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, recs[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return map[string]any{"logs": out, "total": total}
}

type usageCountStat struct {
	Requests int64
	Tokens   int64
}

type usageTrendPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}
