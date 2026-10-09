package ccleft

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClassifyNoCredentials(t *testing.T) {
	r := ClassifyNoCredentials(Claude, "test detail")
	if r.State != StateAuthRequired {
		t.Fatalf("expected StateAuthRequired, got %v", r.State)
	}
	if r.Cause != "no_credentials" {
		t.Fatalf("expected cause no_credentials, got %v", r.Cause)
	}
	if !errors.Is(r.err, ErrNoCredentials) {
		t.Fatalf("expected error wrapping ErrNoCredentials, got %v", r.err)
	}
}

func TestClassifyTokenExpired(t *testing.T) {
	exp := time.Now().Add(-1 * time.Hour)
	// Transient when refresh token present
	r1 := ClassifyTokenExpired(Claude, exp, true, "refresh available")
	if r1.State != StateAuthRequired || r1.Cause != "token_expired" || !r1.Transient() {
		t.Fatalf("expected transient StateAuthRequired, got %v, %v, transient=%v", r1.State, r1.Cause, r1.Transient())
	}

	// Non-transient when refresh token absent
	r2 := ClassifyTokenExpired(Claude, exp, false, "run login")
	if r2.State != StateAuthRequired || r2.Cause != "login_expired" || r2.Transient() {
		t.Fatalf("expected permanent login_expired, got %v, %v, transient=%v", r2.State, r2.Cause, r2.Transient())
	}
}

func TestClassifyNetworkError(t *testing.T) {
	rTimeout := ClassifyNetworkError(Claude, errors.New("deadline exceeded"), true)
	if rTimeout.State != StateError || rTimeout.Cause != "timeout" || !rTimeout.Transient() {
		t.Fatalf("expected timeout error, got %v, %v", rTimeout.State, rTimeout.Cause)
	}

	rNet := ClassifyNetworkError(Claude, errors.New("connection refused"), false)
	if rNet.State != StateError || rNet.Cause != "network" || !rNet.Transient() {
		t.Fatalf("expected network error, got %v, %v", rNet.State, rNet.Cause)
	}
}

func TestClassifySchemaError(t *testing.T) {
	r := ClassifySchemaError(Codex, errors.New("unexpected field"))
	if r.State != StateError || r.Cause != "schema" {
		t.Fatalf("expected schema error, got %v, %v", r.State, r.Cause)
	}
}

func TestClassifyHTTPResponse(t *testing.T) {
	now := time.Now()

	// 429 Rate limited with Retry-After
	rec429 := httptest.NewRecorder()
	rec429.Header().Set("Retry-After", "60")
	rec429.WriteHeader(http.StatusTooManyRequests)
	resp429 := rec429.Result()
	r429 := ClassifyHTTPResponse(Claude, resp429, []byte("too many requests"), now)
	if r429.State != StateRateLimited || r429.Cause != "http_429" || !r429.Transient() {
		t.Fatalf("expected rate limited, got %v, %v", r429.State, r429.Cause)
	}
	if r429.retryAfter != 60*time.Second {
		t.Fatalf("expected 60s retryAfter, got %v", r429.retryAfter)
	}

	// 401 Auth required
	rec401 := httptest.NewRecorder()
	rec401.WriteHeader(http.StatusUnauthorized)
	resp401 := rec401.Result()
	r401 := ClassifyHTTPResponse(Claude, resp401, []byte("unauthorized"), now)
	if r401.State != StateAuthRequired || r401.Cause != "http_401" {
		t.Fatalf("expected auth required, got %v, %v", r401.State, r401.Cause)
	}

	// 503 Server error
	rec503 := httptest.NewRecorder()
	rec503.WriteHeader(http.StatusServiceUnavailable)
	resp503 := rec503.Result()
	r503 := ClassifyHTTPResponse(Claude, resp503, []byte("service unavailable"), now)
	if r503.State != StateError || r503.Cause != "http_503" || !r503.Transient() {
		t.Fatalf("expected transient 503 error, got %v, %v", r503.State, r503.Cause)
	}
}
