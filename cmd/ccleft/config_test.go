package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tuna-os/ccleft"
)

func resetSourcePlanningEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/synthetic/bin")
	for _, key := range append(append([]string{}, envKeys...), homeEnvKeys...) {
		t.Setenv(key, "")
	}
}

func TestBuildSourcesHomes(t *testing.T) {
	root := t.TempDir()
	selfHome := filepath.Join(root, "self")
	resetSourcePlanningEnv(t, selfHome)
	t.Setenv("CODEX_HOME", "/process/codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "/process/claude")
	t.Setenv("COPILOT_GITHUB_TOKEN", "synthetic-copilot-token")
	t.Setenv("KIRO_API_KEY", "synthetic-kiro-key")

	got, err := buildSources(plan{
		homes: []HomeConfig{
			{
				Path:      "~/workspace",
				Label:     "expanded",
				Providers: []string{"claude"},
				Env: map[string]string{
					"CLAUDE_CONFIG_DIR": "/home-specific/claude",
					"CUSTOM_SETTING":    "custom-value",
				},
			},
			{
				Path:      selfHome,
				Label:     "self",
				Providers: []string{"claude", "kiro", "deepseek", "muse", "copilot"},
				Env: map[string]string{
					"CODEX_HOME": "/home-specific/codex",
				},
			},
		},
		providers:    []ccleft.Provider{ccleft.Codex},
		envProviders: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []ccleft.Source{
		{
			Provider: ccleft.Claude,
			Home:     filepath.Join(selfHome, "workspace"),
			Label:    "expanded",
			Env: map[string]string{
				"PATH":              "/synthetic/bin",
				"CLAUDE_CONFIG_DIR": "/home-specific/claude",
				"CUSTOM_SETTING":    "custom-value",
			},
		},
		{
			Provider: ccleft.Claude,
			Home:     selfHome,
			Label:    "self",
			Env: map[string]string{
				"PATH":              "/synthetic/bin",
				"CODEX_HOME":        "/home-specific/codex",
				"CLAUDE_CONFIG_DIR": "/process/claude",
			},
		},
		{
			Provider: ccleft.Copilot,
			Home:     selfHome,
			Label:    "self",
			Env: map[string]string{
				"PATH":                 "/synthetic/bin",
				"CODEX_HOME":           "/home-specific/codex",
				"CLAUDE_CONFIG_DIR":    "/process/claude",
				"COPILOT_GITHUB_TOKEN": "synthetic-copilot-token",
				"KIRO_API_KEY":         "synthetic-kiro-key",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildSourcesEnvProviders(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		resetSourcePlanningEnv(t, t.TempDir())
		t.Setenv("KIRO_API_KEY", "synthetic-kiro-key")

		got, err := buildSources(plan{envProviders: false})
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatalf("sources = %#v, want nil", got)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		resetSourcePlanningEnv(t, t.TempDir())
		t.Setenv("KIRO_API_KEY", "synthetic-kiro-key")
		t.Setenv("DEEPSEEK_API_KEY", "synthetic-deepseek-key")
		t.Setenv("META_API_KEY", "synthetic-meta-key")
		t.Setenv("MUSE_API_KEY", "synthetic-muse-key")
		t.Setenv("COPILOT_GITHUB_TOKEN", "synthetic-copilot-token")

		env := map[string]string{
			"KIRO_API_KEY":         "synthetic-kiro-key",
			"DEEPSEEK_API_KEY":     "synthetic-deepseek-key",
			"META_API_KEY":         "synthetic-meta-key",
			"MUSE_API_KEY":         "synthetic-muse-key",
			"COPILOT_GITHUB_TOKEN": "synthetic-copilot-token",
		}
		want := []ccleft.Source{
			{Provider: ccleft.Kiro, Env: env},
			{Provider: ccleft.DeepSeek, Env: env},
			{Provider: ccleft.Muse, Env: env},
			{Provider: ccleft.Copilot, Env: env},
		}
		got, err := buildSources(plan{envProviders: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sources mismatch\n got: %#v\nwant: %#v", got, want)
		}
	})

	t.Run("explicit filter", func(t *testing.T) {
		resetSourcePlanningEnv(t, t.TempDir())
		t.Setenv("KIRO_API_KEY", "synthetic-kiro-key")
		t.Setenv("DEEPSEEK_API_KEY", "synthetic-deepseek-key")
		env := map[string]string{
			"KIRO_API_KEY":     "synthetic-kiro-key",
			"DEEPSEEK_API_KEY": "synthetic-deepseek-key",
		}

		got, err := buildSources(plan{
			providers:    []ccleft.Provider{ccleft.Claude, ccleft.DeepSeek},
			envProviders: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []ccleft.Source{{Provider: ccleft.DeepSeek, Env: env}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("sources mismatch\n got: %#v\nwant: %#v", got, want)
		}
	})
}

func TestBuildSourcesExtraCredentials(t *testing.T) {
	root := t.TempDir()
	resetSourcePlanningEnv(t, filepath.Join(root, "home"))
	t.Setenv("CCLEFT_TEST_SOURCE_SECRET", "  synthetic-env-secret  ")
	credentialsFile := filepath.Join(root, "credentials")
	if err := os.WriteFile(credentialsFile, []byte("\n synthetic-file-secret \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := buildSources(plan{extra: []SourceConf{
		{
			Provider:       "kiro",
			Home:           "/synthetic/env-home",
			Label:          "env-credentials",
			Env:            map[string]string{"PATH": "/source/bin", "CUSTOM_SETTING": "custom-value"},
			CredentialsEnv: "CCLEFT_TEST_SOURCE_SECRET",
		},
		{
			Provider:        "deepseek",
			Home:            "/synthetic/file-home",
			Label:           "file-credentials",
			CredentialsFile: credentialsFile,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []ccleft.Source{
		{
			Provider:    ccleft.Kiro,
			Home:        "/synthetic/env-home",
			Label:       "env-credentials",
			Env:         map[string]string{"PATH": "/source/bin", "CUSTOM_SETTING": "custom-value"},
			Credentials: "synthetic-env-secret",
		},
		{
			Provider:    ccleft.DeepSeek,
			Home:        "/synthetic/file-home",
			Label:       "file-credentials",
			Env:         map[string]string{"PATH": "/synthetic/bin"},
			Credentials: "synthetic-file-secret",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildSourcesCredentialErrors(t *testing.T) {
	t.Run("empty environment variable", func(t *testing.T) {
		resetSourcePlanningEnv(t, t.TempDir())
		t.Setenv("CCLEFT_TEST_EMPTY_SECRET", "  ")
		_, err := buildSources(plan{extra: []SourceConf{{
			Provider:       "kiro",
			Label:          "team-env",
			CredentialsEnv: "CCLEFT_TEST_EMPTY_SECRET",
		}}})
		if err == nil || !strings.Contains(err.Error(), "source kiro/team-env: env CCLEFT_TEST_EMPTY_SECRET is empty") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing credentials file", func(t *testing.T) {
		root := t.TempDir()
		resetSourcePlanningEnv(t, filepath.Join(root, "home"))
		missing := filepath.Join(root, "missing-credentials")
		_, err := buildSources(plan{extra: []SourceConf{{
			Provider:        "deepseek",
			Label:           "team-file",
			CredentialsFile: missing,
		}}})
		if err == nil {
			t.Fatal("missing credentials file accepted")
		}
		for _, want := range []string{"source deepseek/team-file:", missing} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want substring %q", err, want)
			}
		}
	})
}

func TestBuildSourcesRejectsInvalidProviders(t *testing.T) {
	tests := []struct {
		name string
		plan plan
	}{
		{
			name: "home",
			plan: plan{homes: []HomeConfig{{
				Path:      "/synthetic/home",
				Providers: []string{"not-a-provider"},
			}}},
		},
		{
			name: "extra source",
			plan: plan{extra: []SourceConf{{
				Provider: "not-a-provider",
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetSourcePlanningEnv(t, t.TempDir())
			_, err := buildSources(tt.plan)
			if err == nil || !strings.Contains(err.Error(), `ccleft: unknown provider "not-a-provider"`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
