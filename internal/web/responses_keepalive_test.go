package web

import (
	"net/http/httptest"
	"testing"
)

func TestResponsesKeepaliveUsesAnSSEComment(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := sseSafeRaw(recorder, recorder, ": keepalive\n\n"); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Body.String(); got != ": keepalive\n\n" {
		t.Fatalf("keepalive frame = %q", got)
	}
}
