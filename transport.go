package ccleft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// httpJSON performs req and decodes a 2xx JSON body into out. On failure it
// returns a classified Reading; nil means success.
func (p *Prober) httpJSON(ctx context.Context, pr Provider, req *http.Request, out any) *Reading {
	req = req.WithContext(ctx)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ccleft/"+Version)
	}
	req.Header.Set("Accept", "application/json")
	// Tokens come from files the agent writes. One that cannot be sent would
	// fail in the transport or as a 4xx about the request, and both read as
	// trouble with the endpoint, which every home of the account then shares.
	// It is a bad credential: say so before sending anything.
	for name, vs := range req.Header {
		for _, v := range vs {
			if !sendableHeaderValue(v) {
				r := fail(pr, StateAuthRequired, "invalid_credentials", fmt.Errorf("the %s value cannot be sent: it has a control character or is over %d bytes", name, maxHeaderValue))
				return &r
			}
		}
	}
	resp, err := p.client().Do(req)
	if err != nil {
		cause := "network"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cause = "timeout"
		}
		r := fail(pr, StateError, cause, fmt.Errorf("%s %s: %w", req.Method, redactURL(req), err))
		return &r
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		r := fail(pr, StateError, "network", fmt.Errorf("reading response: %w", err))
		return &r
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		r := classifyHTTP(pr, resp, body, p.now())
		return &r
	}
	if err := json.Unmarshal(body, out); err != nil {
		r := fail(pr, StateError, "schema", fmt.Errorf("decoding %s response: %w (body: %s)", pr, err, snippet(body)))
		return &r
	}
	return nil
}

// maxHeaderValue is far above any real token (a few kilobytes for a JWT) and
// below what servers accept for one header.
const maxHeaderValue = 16 << 10

// sendableHeaderValue reports whether v is a header value net/http will send
// and a server will take: no control characters except tab, and not huge.
func sendableHeaderValue(v string) bool {
	if len(v) > maxHeaderValue {
		return false
	}
	for i := 0; i < len(v); i++ {
		if b := v[i]; (b < ' ' && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}
