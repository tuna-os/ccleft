package ccleft

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Agy (Google Antigravity CLI): there is no HTTP quota endpoint we can call
// with its token, so ccleft runs
//
//	agy --print /usage --output-format json
//
// with HOME set to the source home (never the ccleft process's HOME), a
// minimal explicit environment and a 90 s deadline (the CLI boots slowly;
// Hive's 10 s deadline timed out routinely). The envelope's command.data
// carries groups[].buckets[] with remaining_fraction and reset_time. The
// request consumes no model turn (num_turns 0, total_tokens 0).

func init() { register(Agy, impl{identify: agyIdentify, fetch: agyFetch}) }

func agyTokenPath(src Source) string {
	return src.path(".gemini", "antigravity-cli", "antigravity-oauth-token")
}

func agyIdentify(p *Prober, src Source) (credential, *Reading) {
	if src.Home == "" {
		r := fail(Agy, StateAuthRequired, "no_credentials", fmt.Errorf("%w: agy needs a home directory", ErrNoCredentials))
		return credential{}, &r
	}
	tok := agyTokenPath(src)
	if !fileExists(tok) {
		r := fail(Agy, StateAuthRequired, "no_credentials", fmt.Errorf("%w: %s missing (run agy and log in)", ErrNoCredentials, tok))
		return credential{}, &r
	}
	c := credential{account: fingerprint(Agy, "home:"+filepath.Clean(src.Home))}
	var t map[string]any
	if readJSON(tok, &t) == nil {
		for _, k := range []string{"refresh_token", "refreshToken", "email", "account"} {
			if s, ok := t[k].(string); ok && s != "" {
				c.account = fingerprint(Agy, k+":"+s)
				break
			}
		}
	}
	return c, nil
}

func agyEnv(src Source, bin string) []string {
	path := src.env("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	if dir := filepath.Dir(bin); dir != "." && !strings.Contains(":"+path+":", ":"+dir+":") {
		path = dir + ":" + path
	}
	env := []string{"HOME=" + src.Home, "PATH=" + path, "TERM=dumb", "NO_COLOR=1", "CI=1"}
	for k, v := range src.Env {
		if k == "HOME" || k == "PATH" {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

var loginHints = []string{"log in", "login", "sign in", "sign-in", "authenticate", "not authenticated", "unauthenticated", "oauth"}

func looksLikeLogin(out []byte) bool {
	s := strings.ToLower(string(out))
	for _, h := range loginHints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

func agyFetch(ctx context.Context, p *Prober, src Source, c credential) Reading {
	bin := firstNonEmpty(p.AgyPath, src.env("CCLEFT_AGY"), "agy")
	look := p.LookPath
	if look == nil {
		look = exec.LookPath
	}
	resolved, err := look(bin)
	if err != nil {
		return fail(Agy, StateError, "not_installed", fmt.Errorf("agy binary not found: %w", err))
	}
	run := p.Exec
	if run == nil {
		run = defaultExec
	}
	out, err := run(ctx, agyEnv(src, resolved), resolved, "--print", "/usage", "--output-format", "json")
	jsonPart := extractJSON(out)
	if err != nil && jsonPart == nil {
		if looksLikeLogin(out) {
			return fail(Agy, StateAuthRequired, "login_required", fmt.Errorf("agy asked for a login: %s", snippet(out)))
		}
		if ctx.Err() != nil {
			r := fail(Agy, StateError, "timeout", fmt.Errorf("agy /usage did not finish within %s: %v", p.timeout(Agy), err))
			return r
		}
		return fail(Agy, StateError, "exit_status", fmt.Errorf("agy /usage: %v: %s", err, snippet(out)))
	}
	if jsonPart == nil {
		if looksLikeLogin(out) {
			return fail(Agy, StateAuthRequired, "login_required", fmt.Errorf("agy asked for a login: %s", snippet(out)))
		}
		return fail(Agy, StateError, "schema", fmt.Errorf("agy /usage printed no JSON envelope: %s", snippet(out)))
	}
	return parseAgyUsage(jsonPart)
}

// extractJSON returns the outermost {...} object in CLI output that may carry
// banner lines before or after it.
func extractJSON(out []byte) []byte {
	i := bytes.IndexByte(out, '{')
	j := bytes.LastIndexByte(out, '}')
	if i < 0 || j < i {
		return nil
	}
	return out[i : j+1]
}

type agyEnvelope struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Error    any    `json:"error"`
	Command  *struct {
		Name string `json:"name"`
		Data *struct {
			Groups []struct {
				Name    string `json:"name"`
				Buckets []*struct {
					ID                string  `json:"id"`
					Name              string  `json:"name"`
					Window            string  `json:"window"`
					RemainingFraction flexNum `json:"remaining_fraction"`
					ResetTime         any     `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

func agyKind(window string) Kind {
	switch strings.ToLower(window) {
	case "5h", "five_hour", "short":
		return KindFiveHour
	case "daily", "day", "24h":
		return KindDaily
	case "weekly", "seven_day", "7d":
		return KindWeekly
	case "monthly":
		return KindMonthly
	}
	return Kind(strings.ToLower(window))
}

func parseAgyUsage(b []byte) Reading {
	var env agyEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return fail(Agy, StateError, "schema", fmt.Errorf("agy /usage envelope: %w", err))
	}
	if env.Status != "" && env.Status != "SUCCESS" {
		detail := fmt.Sprintf("agy /usage status %q: %s", env.Status, snippet([]byte(env.Response)))
		if looksLikeLogin([]byte(env.Response)) || looksLikeLogin([]byte(fmt.Sprint(env.Error))) {
			return fail(Agy, StateAuthRequired, "login_required", errors.New(detail))
		}
		return fail(Agy, StateError, "status_"+strings.ToLower(env.Status), errors.New(detail))
	}
	if env.Command == nil || env.Command.Data == nil || len(env.Command.Data.Groups) == 0 {
		return fail(Agy, StateError, "schema", errors.New("agy /usage: no command.data.groups (unrecognized schema)"))
	}
	if env.Command.Name != "" && env.Command.Name != "usage" {
		return fail(Agy, StateError, "schema", fmt.Errorf("agy /usage: envelope is for command %q", env.Command.Name))
	}
	var ws []Window
	for _, g := range env.Command.Data.Groups {
		for _, bk := range g.Buckets {
			if bk == nil || !bk.RemainingFraction.OK {
				continue
			}
			pool := bk.ID
			if i := strings.IndexByte(pool, '-'); i > 0 {
				pool = pool[:i]
			}
			w := pctWindow(bk.ID, agyKind(bk.Window), 100-bk.RemainingFraction.V*100, parseTime(bk.ResetTime))
			// agy's whole quota is split into model pools (gemini-*, 3p-*);
			// each pool binds, so the account reads limited when any pool is
			// out. Consumers that know their model can filter by Scope.
			w.Scope = pool
			ws = append(ws, w)
		}
	}
	if len(ws) == 0 {
		return fail(Agy, StateError, "schema", errors.New("agy /usage: no bucket carried remaining_fraction (unrecognized schema)"))
	}
	return Reading{Windows: ws}
}
