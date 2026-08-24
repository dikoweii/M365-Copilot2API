package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"m365-copilot2api/internal/auth"
)

const maxSSOCookieRequestBytes = 32 << 10

type ssoCookieRequest struct {
	ID      string           `json:"id"`
	Cookies []auth.SSOCookie `json:"cookies"`
}

func (s *Server) accountSSOCookie(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	switch r.Method {
	case http.MethodPut:
		var body ssoCookieRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSSOCookieRequestBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "SSO cookie request is too large")
				return
			}
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid SSO cookie request")
			return
		}
		body.ID = strings.TrimSpace(body.ID)
		if body.ID == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "account id is required")
			return
		}
		if err := s.tokens.SaveSSOCookies(body.ID, body.Cookies); err != nil {
			status := http.StatusBadRequest
			kind := "invalid_request_error"
			if err.Error() == "account not found" {
				status = http.StatusNotFound
			} else if errors.Is(err, auth.ErrSSOKeyUnavailable) {
				status = http.StatusInternalServerError
				kind = "storage_error"
			}
			writeOpenAIError(w, status, kind, err.Error())
			return
		}
		status, err := s.tokens.SSOStatus(body.ID)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "could not read SSO status")
			return
		}
		jsonOut(w, map[string]any{"status": "configured", "sso": status})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "account id is required")
			return
		}
		if err := s.tokens.ClearSSOCookies(id); err != nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "account not found")
			return
		}
		jsonOut(w, map[string]any{"status": "cleared"})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}
