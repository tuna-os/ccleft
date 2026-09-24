package ccleft

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixed test clock: 2026-09-24T19:00:00Z
var testNow = time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b []byte
	switch x := v.(type) {
	case string:
		b = []byte(x)
	case []byte:
		b = x
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// upstream is an httptest server that counts calls and records requests.
type upstream struct {
	*httptest.Server
	calls  atomic.Int32
	mu     sync.Mutex
	reqs   []*http.Request
	bodies [][]byte
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		var body []byte
		if r.Body != nil {
			body, _ = readAll(r)
		}
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(r.Context()))
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) last() (*http.Request, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return nil, nil
	}
	return u.reqs[len(u.reqs)-1], u.bodies[len(u.bodies)-1]
}

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

func serveJSON(status int, body []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// clock is a controllable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testProber(p Provider, u *upstream, clk *clock) *Prober {
	pr := &Prober{Endpoints: map[Provider]string{p: u.URL}}
	if clk != nil {
		pr.Now = clk.Now
	} else {
		pr.Now = func() time.Time { return testNow }
	}
	return pr
}

// claudeHome builds a home with valid Claude credentials for account uuid.
func claudeHome(t *testing.T, accountUUID string) string {
	t.Helper()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      "sk-ant-oat01-TESTTOKEN-" + accountUUID,
			"refreshToken":     "sk-ant-ort01-TESTREFRESH-" + accountUUID,
			"expiresAt":        testNow.Add(4 * time.Hour).UnixMilli(),
			"subscriptionType": "max",
		},
	})
	writeFile(t, filepath.Join(home, ".claude.json"), map[string]any{
		"oauthAccount": map[string]any{"accountUuid": accountUUID, "organizationUuid": "org-" + accountUUID, "emailAddress": "x@example.com"},
	})
	return home
}

// fakeJWT returns an unsigned JWT with the given claims.
func fakeJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func findWindow(t *testing.T, r Reading, id string) Window {
	t.Helper()
	for _, w := range r.Windows {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("window %q not found in %+v", id, r.Windows)
	return Window{}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}
