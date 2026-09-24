package ccleft

import (
	"os/exec"
	"path/filepath"
)

// DetectHome returns a Source for every provider whose credentials are
// present under home (files only; no network, nothing executed). env
// carries per-home overrides such as CODEX_HOME / CLAUDE_CONFIG_DIR.
func DetectHome(home string, env map[string]string) []Source {
	base := Source{Home: home, Env: env}
	var out []Source
	add := func(p Provider) {
		s := base
		s.Provider = p
		out = append(out, s)
	}
	if fileExists(filepath.Join(claudeConfigDir(base), ".credentials.json")) {
		add(Claude)
	}
	if d := codexHome(base); d != "" && fileExists(filepath.Join(d, "auth.json")) {
		add(Codex)
	}
	if fileExists(agyTokenPath(base)) {
		add(Agy)
	}
	if fileExists(base.path(".gemini", "oauth_creds.json")) {
		add(Gemini)
	}
	if cfg := copilotConfigDir(base); cfg != "" {
		for _, f := range []string{
			filepath.Join(cfg, "github-copilot", "apps.json"),
			filepath.Join(cfg, "github-copilot", "hosts.json"),
		} {
			if fileExists(f) {
				add(Copilot)
				break
			}
		}
	}
	return out
}

// DetectEnv returns a Source for every API-key provider whose key is set in
// env (KIRO_API_KEY, DEEPSEEK_API_KEY, META_API_KEY, GH_TOKEN / GITHUB_TOKEN
// / COPILOT_GITHUB_TOKEN).
func DetectEnv(env map[string]string) []Source {
	var out []Source
	has := func(keys ...string) bool {
		for _, k := range keys {
			if env[k] != "" {
				return true
			}
		}
		return false
	}
	add := func(p Provider) { out = append(out, Source{Provider: p, Env: env}) }
	if has("KIRO_API_KEY") {
		add(Kiro)
	}
	if has("DEEPSEEK_API_KEY") {
		add(DeepSeek)
	}
	if has("META_API_KEY", "MUSE_API_KEY") {
		add(Muse)
	}
	if has("COPILOT_GITHUB_TOKEN") {
		add(Copilot)
	}
	return out
}

// AgyAvailable reports whether an agy binary can be found (Detect skips
// nothing on this basis; agy sources without the binary report
// cause not_installed).
func AgyAvailable(path string) bool {
	if path == "" {
		path = "agy"
	}
	_, err := exec.LookPath(path)
	return err == nil
}
