package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeImageModel(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "empty", raw: "", want: imageModelGPTImage2, ok: true},
		{name: "whitespace", raw: " \t", want: imageModelGPTImage2, ok: true},
		{name: "auto", raw: "auto", want: imageModelGPTImage2, ok: true},
		{name: "canonical", raw: imageModelGPTImage2, want: imageModelGPTImage2, ok: true},
		{name: "unsupported", raw: "dall-e-3", ok: false},
		{name: "case sensitive", raw: "GPT-IMAGE-2", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeImageModel(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("normalizeImageModel(%q)=(%q, %t), want (%q, %t)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestImageEndpointsRejectUnsupportedModelBeforeAccountResolution(t *testing.T) {
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		request func(*testing.T) *http.Request
	}{
		{
			name:    "generations",
			handler: (&Server{}).imageGenerations,
			request: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"draw a cat","model":"dall-e-3"}`))
			},
		},
		{
			name:    "edits",
			handler: (&Server{}).imageEdits,
			request: unsupportedModelEditRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handler(w, tt.request(t))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			var response struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("response is not OpenAI-style JSON: %v; body=%s", err, w.Body.String())
			}
			if response.Error.Type != "invalid_request_error" {
				t.Fatalf("error type=%q want invalid_request_error", response.Error.Type)
			}
			if !strings.Contains(response.Error.Message, "gpt-image-2") {
				t.Fatalf("error message=%q does not name the supported model", response.Error.Message)
			}
		})
	}
}

func unsupportedModelEditRequest(t *testing.T) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "make it blue"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("model", "dall-e-3"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("\x89PNG\r\n\x1a\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}
