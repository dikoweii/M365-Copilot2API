package web

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

type keepaliveCaptureWriter struct {
	header http.Header
	body   bytes.Buffer
	writes chan string
}

func newKeepaliveCaptureWriter() *keepaliveCaptureWriter {
	return &keepaliveCaptureWriter{header: make(http.Header), writes: make(chan string, 4)}
}

func (w *keepaliveCaptureWriter) Header() http.Header { return w.header }

func (w *keepaliveCaptureWriter) Write(p []byte) (int, error) {
	w.body.Write(p)
	select {
	case w.writes <- string(p):
	default:
	}
	return len(p), nil
}

func (w *keepaliveCaptureWriter) WriteHeader(int) {}

func (w *keepaliveCaptureWriter) Flush() {}

func TestOpenAIStreamEmitterKeepsSilentStreamAlive(t *testing.T) {
	w := newKeepaliveCaptureWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	delivery := &streamDeliveryState{}
	emitter := newOpenAIStreamEmitter(req, w, w, "chatcmpl_test", "auto", &usageTrace{}, delivery)
	stop := emitter.startKeepalive(5 * time.Millisecond)
	defer stop()

	select {
	case frame := <-w.writes:
		if frame != ": keepalive\n\n" {
			t.Fatalf("keepalive frame = %q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("silent stream did not emit a keepalive frame")
	}

	if delivery.visible {
		t.Fatal("keepalive must not count as visible model output")
	}
}
