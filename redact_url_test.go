package ccleft

import (
	"net/http"
	"testing"
)

func TestRedactURLDropsQueryAndUserinfo(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://user:pw@example.invalid/x?token=SECRET", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := redactURL(req)
	if got != "https://example.invalid/x" {
		t.Fatalf("redactURL = %q, want %q", got, "https://example.invalid/x")
	}
	if req.URL.RawQuery == "" || req.URL.User == nil {
		t.Fatal("redactURL must not mutate the request URL")
	}
}
