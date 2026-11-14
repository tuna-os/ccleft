package ccleft

import (
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestClassifyHTTPResponse(t *testing.T) {
	for _, tc := range []struct {
		status int
		header string
		class  ErrorClass
		retry  bool
		after  int
	}{
		{500, "", ErrorTransient, true, 0},
		{503, "30", ErrorTransient, true, 30},
		{429, "7", ErrorTransient, true, 7},
		{429, "Wed, 21 Oct 2026 07:28:00 GMT", ErrorTransient, true, 0},
		{429, "-5", ErrorTransient, true, 0},
		{401, "", ErrorAuthentication, false, 0},
		{403, "", ErrorAuthentication, false, 0},
		{404, "", ErrorNotFound, false, 0},
		{400, "", ErrorPermanent, false, 0},
		{409, "", ErrorPermanent, false, 0},
		{200, "", ErrorUnknown, false, 0},
		{302, "", ErrorUnknown, false, 0},
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Retry-After", tc.header)
		}
		class, retry, after := ClassifyHTTPResponse(tc.status, h)
		if class != tc.class || retry != tc.retry || after != tc.after {
			t.Errorf("ClassifyHTTPResponse(%d, Retry-After %q) = (%s, %v, %d), want (%s, %v, %d)",
				tc.status, tc.header, class, retry, after, tc.class, tc.retry, tc.after)
		}
	}
}

func TestClassifyNetworkError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ErrorUnknown},
		{"timeout", errors.New("i/o timeout"), ErrorTransient},
		{"connection refused", errors.New("connection refused"), ErrorTransient},
		{"dial", errors.New("dial tcp: lookup example.invalid"), ErrorTransient},
		{"eof", io.EOF, ErrorTransient},
		{"unrelated", errors.New("invalid character 'x'"), ErrorUnknown},
	} {
		if got := ClassifyNetworkError(tc.err); got != tc.want {
			t.Errorf("%s: ClassifyNetworkError = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestStateFromErrorClass(t *testing.T) {
	for class, want := range map[ErrorClass]State{
		ErrorTransient:      StateError,
		ErrorNotFound:       StateUnsupported,
		ErrorAuthentication: StateAuthRequired,
		ErrorPermanent:      StateError,
		ErrorUnknown:        StateError,
		ErrorClass("other"): StateError,
	} {
		if got := StateFromErrorClass(class); got != want {
			t.Errorf("StateFromErrorClass(%q) = %s, want %s", class, got, want)
		}
	}
}

func TestParseProvider(t *testing.T) {
	for _, p := range RegisteredProviders() {
		got, err := ParseProvider(string(p))
		if err != nil || got != p {
			t.Errorf("ParseProvider(%q) = (%q, %v), want (%q, nil)", p, got, err, p)
		}
	}
	for _, s := range []string{"", "nonesuch", "Claude "} {
		if got, err := ParseProvider(s); err == nil || got != "" {
			t.Errorf("ParseProvider(%q) = (%q, %v), want an error", s, got, err)
		}
	}
}
