package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMiddlewareChainFlushesConnectedBeforeBodyCompletes(t *testing.T) {
	handler := recoverPanics(requestID(httpTrace(securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &usageResponseWriter{ResponseWriter: w}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer does not expose http.Flusher")
			return
		}
		if err := sseRaw(context.Background(), w, flusher, ": connected\n\n"); err != nil {
			t.Errorf("connected write: %v", err)
			return
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("data: done\n\n"))
	})))))

	server := httptest.NewServer(handler)
	defer server.Close()
	started := time.Now()
	resp, err := server.Client().Get(server.URL + "/v1/flush-test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != ": connected\n" {
		t.Fatalf("first line = %q", line)
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("connected frame was buffered for %s", elapsed)
	}
}
