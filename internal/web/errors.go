package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func logOAuthError(stage string, err error) {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		log.Printf("oauth_error stage=%s error=%q aadsts=%q http_status=%d correlation_id=%q trace_id=%q", stage, oauthErr.Code, oauthErr.AADSTS, oauthErr.HTTPStatus, oauthErr.CorrelationID, oauthErr.TraceID)
		return
	}
	log.Printf("oauth_error stage=%s error=%q", stage, "request_failed")
}

// upstreamError keeps transport details, including URLs and credentials, out
// of client-visible responses while retaining a server-side diagnostic.
func upstreamError(err error) string {
	if err == nil {
		return "upstream request failed"
	}
	log.Printf("upstream request failed: %v", err)
	if IsAuthFailure(err) {
		return "upstream authentication failed; refresh or replace the selected account"
	}
	var interrupted *chathub.StreamInterruptedError
	if errors.As(err, &interrupted) {
		return "upstream stream was interrupted; retry or resume the request"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "upstream request timed out; retry the request"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "upstream transport failed; retry the request"
	}
	return "upstream request failed"
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// everything else is 502. Unknown upstream failures must never leak internals.
func upstreamStatus(err error) int {
	if IsRateLimited(err) {
		return http.StatusTooManyRequests
	}
	if IsAuthFailure(err) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	var httpErr *UpstreamHTTPError
	if errors.As(err, &httpErr) && (httpErr.Status == http.StatusRequestTimeout || httpErr.Status == http.StatusGatewayTimeout) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint for rate limits so clients can back off.
func writeUpstreamError(w http.ResponseWriter, err error) {
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(rateLimitCooldown.Seconds())))
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}

const statusClientClosedRequest = 499

// writePreStreamUpstreamError is used before any streaming response bytes have
// been committed, so callers still receive the real HTTP failure status.
func writePreStreamUpstreamError(w http.ResponseWriter, r *http.Request, trace *usageTrace, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		trace.fail(statusClientClosedRequest, "client_cancelled", "client disconnected before the response completed")
		return
	}
	status := upstreamStatus(err)
	trace.fail(status, usageErrorType(status), sanitizePublicInternalText(upstreamError(err)))
	writeUpstreamError(w, err)
}
