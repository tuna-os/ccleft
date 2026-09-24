package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tuna-os/ccleft"
	"gopkg.in/yaml.v3"
)

// Config is the YAML file accepted by --config.
//
//	addr: ":9464"
//	refresh: 5m
//	env_providers: true        # probe KIRO_API_KEY / DEEPSEEK_API_KEY / ... from ccleft's env
//	homes:
//	  - path: /data/home
//	    label: hive-main
//	    providers: [claude, codex, agy]   # omit to auto-detect
//	    env: {CODEX_HOME: /data/home/.codex}
//	sources:
//	  - provider: kiro
//	    label: team-2
//	    credentials_env: KIRO_API_KEY_TEAM2   # env var holding the key
//	  - provider: deepseek
//	    credentials_file: /run/secrets/deepseek
//
// Secrets never go in the file itself: credentials are referenced by env
// var name or file path.
type Config struct {
	Addr         string        `yaml:"addr"`
	Refresh      time.Duration `yaml:"refresh"`
	EnvProviders *bool         `yaml:"env_providers"`
	Homes        []HomeConfig  `yaml:"homes"`
	Sources      []SourceConf  `yaml:"sources"`
	// MinInterval overrides the per-account upstream interval per provider.
	MinInterval map[string]time.Duration `yaml:"min_interval"`
	AgyPath     string                   `yaml:"agy_path"`
}

type HomeConfig struct {
	Path      string            `yaml:"path"`
	Label     string            `yaml:"label"`
	Providers []string          `yaml:"providers"`
	Env       map[string]string `yaml:"env"`
}

type SourceConf struct {
	Provider        string            `yaml:"provider"`
	Home            string            `yaml:"home"`
	Label           string            `yaml:"label"`
	Env             map[string]string `yaml:"env"`
	CredentialsEnv  string            `yaml:"credentials_env"`
	CredentialsFile string            `yaml:"credentials_file"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// envKeys are the process-environment variables forwarded to sources.
var envKeys = []string{"KIRO_API_KEY", "DEEPSEEK_API_KEY", "META_API_KEY", "MUSE_API_KEY", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "GEMINI_API_KEY"}

// homeEnvKeys only make sense for the process's own home.
var homeEnvKeys = []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "GH_CONFIG_DIR"}

func processEnv(keys []string) map[string]string {
	m := map[string]string{}
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	return m
}

type plan struct {
	homes        []HomeConfig
	providers    []ccleft.Provider // explicit filter (empty = detect)
	envProviders bool
	extra        []SourceConf
}

// buildSources resolves homes + providers into Sources. Called on every
// refresh so new logins are picked up.
func buildSources(pl plan) ([]ccleft.Source, error) {
	var out []ccleft.Source
	selfHome, _ := os.UserHomeDir()
	path := os.Getenv("PATH")
	keyEnv := processEnv(envKeys)
	want := func(p ccleft.Provider, filter []ccleft.Provider) bool {
		if len(filter) == 0 {
			return true
		}
		for _, f := range filter {
			if f == p {
				return true
			}
		}
		return false
	}
	isEnvProvider := func(p ccleft.Provider) bool {
		return p == ccleft.Kiro || p == ccleft.DeepSeek || p == ccleft.Muse
	}
	for _, h := range pl.homes {
		home := h.Path
		if strings.HasPrefix(home, "~/") {
			home = filepath.Join(selfHome, home[2:])
		}
		env := map[string]string{"PATH": path}
		if filepath.Clean(home) == filepath.Clean(selfHome) {
			for k, v := range processEnv(homeEnvKeys) {
				env[k] = v
			}
		}
		for k, v := range h.Env {
			env[k] = v
		}
		filter := pl.providers
		if len(h.Providers) > 0 {
			filter = nil
			for _, s := range h.Providers {
				p, err := ccleft.ParseProvider(s)
				if err != nil {
					return nil, err
				}
				filter = append(filter, p)
			}
		}
		if len(filter) == 0 {
			for _, s := range ccleft.DetectHome(home, env) {
				s.Label = h.Label
				out = append(out, s)
			}
			continue
		}
		for _, p := range filter {
			if isEnvProvider(p) {
				continue // added once below, not per home
			}
			e := env
			if p == ccleft.Copilot {
				e = merge(env, keyEnv)
			}
			out = append(out, ccleft.Source{Provider: p, Home: home, Env: e, Label: h.Label})
		}
	}
	if pl.envProviders {
		if len(pl.providers) == 0 {
			out = append(out, ccleft.DetectEnv(keyEnv)...)
		} else {
			for _, p := range pl.providers {
				if isEnvProvider(p) {
					out = append(out, ccleft.Source{Provider: p, Env: keyEnv})
				}
			}
		}
	}
	for _, sc := range pl.extra {
		p, err := ccleft.ParseProvider(sc.Provider)
		if err != nil {
			return nil, err
		}
		if !want(p, pl.providers) {
			continue
		}
		s := ccleft.Source{Provider: p, Home: sc.Home, Env: merge(map[string]string{"PATH": path}, sc.Env), Label: sc.Label}
		switch {
		case sc.CredentialsEnv != "":
			s.Credentials = strings.TrimSpace(os.Getenv(sc.CredentialsEnv))
			if s.Credentials == "" {
				return nil, fmt.Errorf("source %s/%s: env %s is empty", sc.Provider, sc.Label, sc.CredentialsEnv)
			}
		case sc.CredentialsFile != "":
			b, err := os.ReadFile(sc.CredentialsFile)
			if err != nil {
				return nil, fmt.Errorf("source %s/%s: %w", sc.Provider, sc.Label, err)
			}
			s.Credentials = strings.TrimSpace(string(b))
		}
		out = append(out, s)
	}
	return out, nil
}

func merge(a, b map[string]string) map[string]string {
	m := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}
