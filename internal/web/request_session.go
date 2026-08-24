package web

import (
	"net/http"
	"strings"
)

var requestSessionHeaderNames = [...]string{
	"X-M365-Session-Id",
	"X-Session-Id",
	"X-Claude-Code-Session-Id",
	"Session-Id",
}

func requestSessionKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, name := range requestSessionHeaderNames {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func explicitRequestSessionKey(r *http.Request, body *oaiReq) string {
	if body != nil {
		if value := strings.TrimSpace(body.SessionKey); value != "" {
			return value
		}
	}
	return requestSessionKey(r)
}
