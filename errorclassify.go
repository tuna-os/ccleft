package ccleft

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// ClassifyNoCredentials returns a standardized StateAuthRequired reading
// when required credential files, environment variables, or tokens are missing.
func ClassifyNoCredentials(p Provider, detail string) Reading {
	var err error
	if detail != "" {
		err = fmt.Errorf("%w: %s", ErrNoCredentials, detail)
	} else {
		err = ErrNoCredentials
	}
	return fail(p, StateAuthRequired, "no_credentials", err)
}

// ClassifyTokenExpired returns a StateAuthRequired reading when an access token
// is expired. If hasRefreshToken is true, the error is marked transient because
// the CLI tool or runtime can refresh it on the next normal execution.
func ClassifyTokenExpired(p Provider, exp time.Time, hasRefreshToken bool, hint string) Reading {
	if hasRefreshToken {
		msg := fmt.Sprintf("access token expired at %s; refresh token present", exp.UTC().Format(time.RFC3339))
		if hint != "" {
			msg += ", " + hint
		}
		r := fail(p, StateAuthRequired, "token_expired", errors.New(msg))
		r.transient = true
		return r
	}
	msg := "access token expired and no refresh token"
	if hint != "" {
		msg += ": " + hint
	}
	return fail(p, StateAuthRequired, "login_expired", errors.New(msg))
}

// ClassifySchemaError returns a StateError reading with cause "schema" when
// the payload returned by a provider does not conform to expectations.
func ClassifySchemaError(p Provider, err error) Reading {
	return fail(p, StateError, "schema", err)
}

// ClassifyNetworkError returns a StateError reading for network transport failures,
// distinguishing timeouts from generic connection errors.
func ClassifyNetworkError(p Provider, err error, isTimeout bool) Reading {
	cause := "network"
	if isTimeout {
		cause = "timeout"
	}
	return fail(p, StateError, cause, err)
}

// ClassifyHTTPResponse centralizes classification of HTTP response codes and headers.
func ClassifyHTTPResponse(p Provider, resp *http.Response, body []byte, now time.Time) Reading {
	code := resp.StatusCode
	err := fmt.Errorf("HTTP %d: %s", code, snippet(body))
	cause := "http_" + strconv.Itoa(code)
	switch {
	case code == http.StatusTooManyRequests:
		r := fail(p, StateRateLimited, cause, err)
		r.retryAfter = ParseRetryAfter(resp.Header.Get("Retry-After"), now)
		return r
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fail(p, StateAuthRequired, cause, err)
	case code >= 500:
		r := fail(p, StateError, cause, err)
		if ra := ParseRetryAfter(resp.Header.Get("Retry-After"), now); ra > 0 {
			r.retryAfter = ra
		}
		return r
	default:
		return fail(p, StateError, cause, err)
	}
}
