package ccleft

import (
	"net/http"
	"strconv"
	"strings"
)

// ErrorClass categorizes probe failures for retry/fallback decisions.
type ErrorClass string

const (
	// ErrorTransient: retry-able failure (5xx, timeout, rate limit, network).
	ErrorTransient ErrorClass = "transient"
	// ErrorNotFound: probe resource does not exist (404).
	ErrorNotFound ErrorClass = "not_found"
	// ErrorAuthentication: credential invalid or revoked (401, 403).
	ErrorAuthentication ErrorClass = "authentication"
	// ErrorPermanent: non-retryable client error (4xx except 401/403/429).
	ErrorPermanent ErrorClass = "permanent"
	// ErrorUnknown: could not classify (default fallback).
	ErrorUnknown ErrorClass = "unknown"
)

// ClassifyHTTPResponse categorizes an HTTP response for retry decisions.
// Returns (ErrorClass, shouldRetry, hasRetryAfter, retryAfterSeconds).
//
// This replaces scattered HTTP status checks across provider implementations
// with a unified classification strategy.
func ClassifyHTTPResponse(statusCode int, headers http.Header) (ErrorClass, bool, int) {
	// 5xx: transient server errors, retry-able.
	if statusCode >= 500 && statusCode < 600 {
		return ErrorTransient, true, retryAfterSeconds(headers)
	}

	// Rate limit: explicitly transient and retry-able.
	if statusCode == 429 {
		return ErrorTransient, true, retryAfterSeconds(headers)
	}

	// Authentication failures: permanent (fix credentials or request access).
	if statusCode == 401 || statusCode == 403 {
		return ErrorAuthentication, false, 0
	}

	// Not found: permanent (endpoint/resource does not exist).
	if statusCode == 404 {
		return ErrorNotFound, false, 0
	}

	// Other 4xx: permanent client errors (bad request, conflict, etc).
	if statusCode >= 400 && statusCode < 500 {
		return ErrorPermanent, false, 0
	}

	// 2xx/3xx success codes should not reach this function.
	// If they do, treat as unknown (caller should check for success first).
	return ErrorUnknown, false, 0
}

// ClassifyNetworkError categorizes non-HTTP errors (timeouts, connection failures).
// All network-level errors are treated as transient.
func ClassifyNetworkError(err error) ErrorClass {
	if err == nil {
		return ErrorUnknown
	}

	// Timeouts, connection resets, DNS failures are transient.
	errStr := err.Error()
	if strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "connection") ||
		strings.Contains(errStr, "dial") ||
		strings.Contains(errStr, "EOF") {
		return ErrorTransient
	}

	return ErrorUnknown
}

// retryAfterSeconds extracts the Retry-After header value (seconds).
// Returns 0 if header is absent or unparseable.
func retryAfterSeconds(headers http.Header) int {
	if val := headers.Get("Retry-After"); val != "" {
		// Try parsing as seconds (integer).
		if sec, err := strconv.Atoi(val); err == nil && sec > 0 {
			return sec
		}
		// Could also be an HTTP-date, but for now we ignore it.
	}
	return 0
}

// StateFromErrorClass maps error classification to a Reading State.
// Used by providers to consistently report errors.
func StateFromErrorClass(ec ErrorClass) State {
	switch ec {
	case ErrorTransient:
		return StateError // Transient errors get StateError (retry via Client)
	case ErrorNotFound:
		return StateUnsupported // Provider endpoint does not exist
	case ErrorAuthentication:
		return StateAuthRequired // Credential issue
	case ErrorPermanent:
		return StateError // Permanent 4xx treated as error (no retry)
	default:
		return StateError
	}
}
