package ccleft

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Source names one credential to probe.
//
// Every filesystem path is derived from Home (or an Env override such as
// CODEX_HOME or CLAUDE_CONFIG_DIR); the process's own $HOME and environment
// are never consulted, so one ccleft process can serve many agent homes.
type Source struct {
	Provider Provider `json:"provider" yaml:"provider"`
	// Home is the agent's home directory (where .claude/, .codex/, ... live).
	Home string `json:"home,omitempty" yaml:"home,omitempty"`
	// Env holds provider-relevant variables (CODEX_HOME, CLAUDE_CONFIG_DIR,
	// KIRO_API_KEY, DEEPSEEK_API_KEY, GH_TOKEN, ...). It is NOT the process
	// environment unless the caller copies it in.
	Env map[string]string `json:"-" yaml:"env,omitempty"`
	// Credentials, when set, overrides credential discovery: an access token
	// (Claude, Codex), API key (Kiro, DeepSeek) or GitHub token (Copilot).
	Credentials string `json:"-" yaml:"-"`
	// Label is a free-form display name (optional).
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
}

func (s Source) env(k string) string {
	if s.Env == nil {
		return ""
	}
	return strings.TrimSpace(s.Env[k])
}

// path joins elements under Home; empty when Home is unset.
func (s Source) path(elem ...string) string {
	if s.Home == "" {
		return ""
	}
	return filepath.Join(append([]string{s.Home}, elem...)...)
}

// fingerprint derives the non-secret account key: 16 hex chars of
// sha256("ccleft\x00<provider>\x00<identity>"). The identity may be a secret
// (an API key); the truncated, domain-separated hash is not reversible.
func fingerprint(p Provider, identity string) string {
	sum := sha256.Sum256([]byte("ccleft\x00" + string(p) + "\x00" + identity))
	return hex.EncodeToString(sum[:])[:16]
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
