package ccleft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Version is the library version, sent in the User-Agent.
var Version = "0.1.0-dev"

// ExecFunc runs a CLI with exactly the given environment (never inheriting the
// caller's) and returns combined output.
type ExecFunc func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)

// Prober performs one-shot measurements. The zero value is usable; fields
// override defaults (mostly for tests and custom deployments).
type Prober struct {
	// HTTPClient is used for every HTTP probe. Default: a client with no
	// global timeout (per-provider timeouts come from Timeouts via ctx).
	HTTPClient *http.Client
	// Endpoints overrides a provider's base URL (e.g. an httptest server).
	Endpoints map[Provider]string
	// Timeouts overrides the per-provider deadline.
	Timeouts map[Provider]time.Duration
	// Exec runs CLI probes (agy). Default: os/exec with WaitDelay.
	Exec ExecFunc
	// AgyPath is the agy binary (default "agy" on PATH).
	AgyPath string
	// LookPath resolves binaries (default exec.LookPath).
	LookPath func(string) (string, error)
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// DefaultProber backs the package-level Probe.
var DefaultProber = &Prober{}

// Probe measures one source once with DefaultProber. It never writes to
// credential files and never panics; failures come back as a Reading whose
// State/Cause/Message say what went wrong.
func Probe(ctx context.Context, src Source) Reading { return DefaultProber.Probe(ctx, src) }

// DefaultTimeouts are the per-provider probe deadlines.
var DefaultTimeouts = map[Provider]time.Duration{
	Claude: 20 * time.Second, Codex: 20 * time.Second, Kiro: 20 * time.Second,
	Copilot: 20 * time.Second, DeepSeek: 20 * time.Second,
	// agy boots a full CLI; 10s (Hive's old value) timed out routinely.
	Agy: 90 * time.Second,
}

// credential is what identify() resolves from local files, with no network.
type credential struct {
	token   string
	account string // fingerprint
	plan    string
	extra   map[string]string
}

type impl struct {
	identify func(p *Prober, src Source) (credential, *Reading)
	fetch    func(ctx context.Context, p *Prober, src Source, c credential) Reading
}

var impls = map[Provider]impl{}

func register(p Provider, i impl) { impls[p] = i }

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) timeout(pr Provider) time.Duration {
	if d, ok := p.Timeouts[pr]; ok && d > 0 {
		return d
	}
	if d, ok := DefaultTimeouts[pr]; ok {
		return d
	}
	return 20 * time.Second
}

func (p *Prober) endpoint(pr Provider, def string) string {
	if u, ok := p.Endpoints[pr]; ok && u != "" {
		return strings.TrimRight(u, "/")
	}
	return def
}

func (p *Prober) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// Identify resolves the account fingerprint for src from local credentials
// only (no network). When no usable credential exists it returns a terminal
// Reading (auth_required / unsupported) instead.
func (p *Prober) Identify(src Source) (account string, terminal *Reading) {
	i, ok := impls[src.Provider]
	if !ok {
		r := fail(src.Provider, StateError, "unknown_provider", fmt.Errorf("unknown provider %q", src.Provider))
		return "", &r
	}
	c, t := i.identify(p, src)
	if t != nil {
		p.finish(t, src, c)
		return c.account, t
	}
	return c.account, nil
}

// Probe measures src once.
func (p *Prober) Probe(ctx context.Context, src Source) Reading {
	i, ok := impls[src.Provider]
	if !ok {
		return fail(src.Provider, StateError, "unknown_provider", fmt.Errorf("unknown provider %q", src.Provider))
	}
	c, t := i.identify(p, src)
	if t != nil {
		p.finish(t, src, c)
		return *t
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout(src.Provider))
	defer cancel()
	r := i.fetch(ctx, p, src, c)
	p.finish(&r, src, c)
	return r
}

func (p *Prober) finish(r *Reading, src Source, c credential) {
	r.Provider = src.Provider
	if r.Account == "" {
		r.Account = c.account
	}
	if r.Plan == "" {
		r.Plan = c.plan
	}
	if r.Windows == nil {
		r.Windows = []Window{}
	}
	if r.FetchedAt.IsZero() {
		r.FetchedAt = p.now().UTC()
	}
	if r.State == "" {
		r.State = deriveState(r.Windows)
	}
	if r.RetryAt == nil && r.retryAfter > 0 {
		t := r.FetchedAt.Add(r.retryAfter)
		r.RetryAt = &t
	}
}

// httpJSON performs req and decodes a 2xx JSON body into out. On failure it
// returns a classified Reading; nil means success.
func (p *Prober) httpJSON(ctx context.Context, pr Provider, req *http.Request, out any) *Reading {
	req = req.WithContext(ctx)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "ccleft/"+Version)
	}
	req.Header.Set("Accept", "application/json")
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

func classifyHTTP(pr Provider, resp *http.Response, body []byte, now time.Time) Reading {
	code := resp.StatusCode
	err := fmt.Errorf("HTTP %d: %s", code, snippet(body))
	cause := "http_" + strconv.Itoa(code)
	switch {
	case code == http.StatusTooManyRequests:
		r := fail(pr, StateRateLimited, cause, err)
		r.retryAfter = ParseRetryAfter(resp.Header.Get("Retry-After"), now)
		return r
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fail(pr, StateAuthRequired, cause, err)
	case code >= 500:
		r := fail(pr, StateError, cause, err)
		if ra := ParseRetryAfter(resp.Header.Get("Retry-After"), now); ra > 0 {
			r.retryAfter = ra
		}
		return r
	default:
		return fail(pr, StateError, cause, err)
	}
}

// ParseRetryAfter parses a Retry-After header (delta-seconds or HTTP-date).
// Returns 0 when absent or unparseable.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// snippet renders a short single-line body excerpt for error messages. The
// quota endpoints do not echo credentials; bearer-looking tokens are masked
// anyway.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return maskSecrets(s)
}

func maskSecrets(s string) string {
	for _, pfx := range []string{"Bearer ", "ksk_", "sk-", "ghp_", "gho_", "ghu_", "github_pat_"} {
		for {
			i := strings.Index(s, pfx)
			if i < 0 {
				break
			}
			j := i + len(pfx)
			for j < len(s) && s[j] != ' ' && s[j] != '"' && s[j] != ',' {
				j++
			}
			s = s[:i] + "<redacted>" + s[j:]
		}
	}
	return s
}

func redactURL(req *http.Request) string {
	u := *req.URL
	u.RawQuery = ""
	u.User = nil
	return u.String()
}

// flexNum decodes a JSON number, a numeric string, or null. Upstream schemas
// drift between the two (codex credits.balance is "0" on one day and 0 on
// another); a fixed Go type turns that into a hard parse failure.
type flexNum struct {
	V  float64
	OK bool
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = flexNum{}
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*f = flexNum{}
			return nil
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return fmt.Errorf("flexNum: %q is not numeric", str)
		}
		*f = flexNum{V: v, OK: true}
		return nil
	}
	if s == "true" || s == "false" {
		return fmt.Errorf("flexNum: boolean %s is not numeric", s)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexNum{V: v, OK: true}
	return nil
}

func (f flexNum) ptr() *float64 {
	if !f.OK {
		return nil
	}
	return f64(f.V)
}

// parseTime accepts RFC3339 strings, epoch seconds or epoch milliseconds.
func parseTime(v any) *time.Time {
	var t time.Time
	switch x := v.(type) {
	case string:
		x = strings.TrimSpace(x)
		if x == "" {
			return nil
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if pt, err := time.Parse(layout, x); err == nil {
				t = pt
				break
			}
		}
		if t.IsZero() {
			if n, err := strconv.ParseFloat(x, 64); err == nil {
				return parseTime(n)
			}
			return nil
		}
	case float64:
		if x <= 0 {
			return nil
		}
		if x > 1e12 {
			t = time.UnixMilli(int64(x))
		} else {
			sec := int64(x)
			t = time.Unix(sec, int64((x-float64(sec))*1e9))
		}
	case int64:
		return parseTime(float64(x))
	case flexNum:
		if !x.OK {
			return nil
		}
		return parseTime(x.V)
	default:
		return nil
	}
	t = t.UTC()
	return &t
}

// defaultExec runs a CLI with an explicit environment and bounded waits. A
// grandchild holding the output pipe cannot wedge the caller: WaitDelay
// force-closes the pipes after the context ends.
func defaultExec(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}
