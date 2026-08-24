package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeConfig(t, `
repo = "acme/service"
base = "release"
interval = "2m"
bots = ["deps-bot", "release-bot"]
`)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Repo != "acme/service" || cfg.Base != "release" || cfg.Interval != "2m" {
		t.Errorf("cfg = %+v", cfg)
	}
	if len(cfg.Bots) != 2 || cfg.Bots[0] != "deps-bot" {
		t.Errorf("bots = %v", cfg.Bots)
	}
}

func TestLoadConfigMissingFileIsFine(t *testing.T) {
	// The tool must work with no config at all.
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("a missing config must not be an error: %v", err)
	}
	if cfg.Repo != "" {
		t.Errorf("cfg = %+v, want zero", cfg)
	}

	if _, err := loadConfig(""); err != nil {
		t.Errorf("an empty path must not be an error: %v", err)
	}
}

// Malformed TOML is an error rather than being ignored: silently discarding it
// would leave the user wondering why their settings do nothing.
func TestLoadConfigRejectsMalformedTOML(t *testing.T) {
	path := writeConfig(t, "repo = \nbots = [")

	if _, err := loadConfig(path); err == nil {
		t.Fatal("want an error for malformed TOML")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("the error should name the file: %v", err)
	}
}

func TestResolvePrefersFlagsOverConfig(t *testing.T) {
	cfg := config{Repo: "acme/service", Base: "release", Interval: "5m"}
	set := map[string]bool{"repo": true, "base": true, "interval": true}

	s, err := resolve(cfg, set, "other/repo", "develop", 10*time.Second, "carol")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.owner != "other" || s.name != "repo" {
		t.Errorf("repo = %s/%s, want other/repo", s.owner, s.name)
	}
	if s.base != "develop" {
		t.Errorf("base = %q, want develop", s.base)
	}
	if s.interval != 10*time.Second {
		t.Errorf("interval = %s, want 10s", s.interval)
	}
}

func TestResolveFallsBackToConfig(t *testing.T) {
	cfg := config{Repo: "acme/service", Base: "release", Interval: "5m", Bots: []string{"deps-bot"}}

	// No flags passed: the file supplies everything.
	s, err := resolve(cfg, map[string]bool{}, "", "main", time.Minute, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.owner != "acme" || s.name != "service" {
		t.Errorf("repo = %s/%s", s.owner, s.name)
	}
	if s.base != "release" {
		t.Errorf("base = %q, want release", s.base)
	}
	if s.interval != 5*time.Minute {
		t.Errorf("interval = %s, want 5m", s.interval)
	}
	if !s.bots["deps-bot"] {
		t.Errorf("bots = %v", s.bots)
	}
}

func TestResolveKeepsFlagDefaultsWhenConfigSilent(t *testing.T) {
	s, err := resolve(config{Repo: "acme/service"}, map[string]bool{}, "", "main", time.Minute, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.base != "main" || s.interval != time.Minute {
		t.Errorf("defaults lost: base %q interval %s", s.base, s.interval)
	}
	if len(s.bots) != 0 {
		t.Errorf("bots = %v, want empty", s.bots)
	}
}

func TestResolveErrors(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config
		repo     string
		interval time.Duration
		wantErr  string
	}{
		{
			name:    "no repo anywhere",
			repo:    "",
			wantErr: "-repo is required",
		},
		{
			name:    "malformed repo in config",
			cfg:     config{Repo: "service"},
			wantErr: "is not owner/name",
		},
		{
			name:    "unparseable interval in config",
			cfg:     config{Repo: "acme/service", Interval: "soon"},
			wantErr: "config interval",
		},
		{
			name:    "non-positive interval",
			cfg:     config{Repo: "acme/service", Interval: "0s"},
			wantErr: "must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interval := tt.interval
			if interval == 0 {
				interval = time.Minute
			}
			_, err := resolve(tt.cfg, map[string]bool{}, tt.repo, "main", interval, "")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveDropsEmptyBotLogins(t *testing.T) {
	cfg := config{Repo: "acme/service", Bots: []string{"deps-bot", ""}}

	s, err := resolve(cfg, map[string]bool{}, "", "main", time.Minute, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(s.bots) != 1 || !s.bots["deps-bot"] {
		t.Errorf("bots = %v, want just deps-bot", s.bots)
	}
	// An empty login would otherwise match every PR whose author failed to decode.
	if s.bots[""] {
		t.Error("an empty login must never be a bot")
	}
}

// writeAt writes a file, creating parent directories.
func writeAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMergeOverridesOnlyWhatIsSet(t *testing.T) {
	base := config{Repo: "acme/service", Base: "main", Interval: "1m", Account: "alice", Bots: []string{"deps-bot"}}

	// A per-repo file naming only the account must leave everything else alone.
	got := merge(base, config{Account: "bob"})
	if got.Account != "bob" {
		t.Errorf("account = %q, want bob", got.Account)
	}
	if got.Repo != "acme/service" || got.Base != "main" || got.Interval != "1m" {
		t.Errorf("untouched fields were lost: %+v", got)
	}
	if len(got.Bots) != 1 || got.Bots[0] != "deps-bot" {
		t.Errorf("bots should be inherited: %v", got.Bots)
	}

	// An empty override changes nothing.
	if got := merge(base, config{}); got.Repo != base.Repo || got.Account != base.Account {
		t.Errorf("an empty override must be a no-op: %+v", got)
	}

	// A non-empty bots list replaces rather than appends.
	got = merge(base, config{Bots: []string{"other-bot"}})
	if len(got.Bots) != 1 || got.Bots[0] != "other-bot" {
		t.Errorf("bots = %v, want the override to replace", got.Bots)
	}
}

func TestFindLocalConfigWalksUp(t *testing.T) {
	root := t.TempDir()
	writeAt(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeAt(t, filepath.Join(root, localConfigName), "repo = \"acme/service\"\n")
	deep := filepath.Join(root, "pkg", "inner")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got := findLocalConfig(deep)
	if got != filepath.Join(root, localConfigName) {
		t.Errorf("findLocalConfig() = %q, want the repo-root file", got)
	}

	// The nearest one wins.
	nearer := filepath.Join(root, "pkg", localConfigName)
	writeAt(t, nearer, "account = \"bob\"\n")
	if got := findLocalConfig(deep); got != nearer {
		t.Errorf("findLocalConfig() = %q, want the nearest file %q", got, nearer)
	}
}

// The search stops at the repository root, so an unrelated parent directory
// cannot silently supply settings.
func TestFindLocalConfigStopsAtRepoRoot(t *testing.T) {
	outer := t.TempDir()
	writeAt(t, filepath.Join(outer, localConfigName), "account = \"stranger\"\n")

	repo := filepath.Join(outer, "repo")
	writeAt(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")

	if got := findLocalConfig(repo); got != "" {
		t.Errorf("findLocalConfig() = %q, want nothing beyond the repo root", got)
	}
}

func TestFindLocalConfigWhenAbsent(t *testing.T) {
	if got := findLocalConfig(t.TempDir()); got != "" {
		t.Errorf("findLocalConfig() = %q, want empty", got)
	}
	if got := findLocalConfig(""); got != "" {
		t.Errorf("findLocalConfig(\"\") = %q, want empty", got)
	}
}

func TestLoadConfigsLayersLocalOverGlobal(t *testing.T) {
	globalPath := writeConfig(t, `
repo = "acme/service"
base = "main"
interval = "1m"
account = "alice"
`)

	repo := t.TempDir()
	writeAt(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeAt(t, filepath.Join(repo, localConfigName), `
repo = "other/project"
account = "bob"
`)

	cfg, sources, err := loadConfigs(globalPath, repo)
	if err != nil {
		t.Fatalf("loadConfigs: %v", err)
	}
	if cfg.Repo != "other/project" || cfg.Account != "bob" {
		t.Errorf("the local file should win: %+v", cfg)
	}
	if cfg.Base != "main" || cfg.Interval != "1m" {
		t.Errorf("unset local fields should fall back to global: %+v", cfg)
	}
	if len(sources) != 2 || sources[0] != globalPath {
		t.Errorf("sources = %v, want global then local", sources)
	}
}

func TestLoadConfigsWithNeitherFile(t *testing.T) {
	cfg, sources, err := loadConfigs(filepath.Join(t.TempDir(), "absent.toml"), t.TempDir())
	if err != nil {
		t.Fatalf("loadConfigs: %v", err)
	}
	if !isEmpty(cfg) {
		t.Errorf("cfg = %+v, want empty", cfg)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want none", sources)
	}
}

func TestLoadConfigsReportsMalformedLocalFile(t *testing.T) {
	repo := t.TempDir()
	writeAt(t, filepath.Join(repo, localConfigName), "repo = [")

	if _, _, err := loadConfigs("", repo); err == nil {
		t.Fatal("want an error for a malformed local config")
	}
}

func TestResolveAccountPrecedence(t *testing.T) {
	// Flag beats config.
	s, err := resolve(config{Repo: "acme/service", Account: "alice"}, map[string]bool{"account": true}, "", "main", time.Minute, "carol")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.account != "carol" {
		t.Errorf("account = %q, want the flag to win", s.account)
	}

	// Config used when the flag was not passed.
	s, err = resolve(config{Repo: "acme/service", Account: "alice"}, map[string]bool{}, "", "main", time.Minute, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.account != "alice" {
		t.Errorf("account = %q, want alice", s.account)
	}

	// Unset everywhere means "use whichever account is active".
	s, err = resolve(config{Repo: "acme/service"}, map[string]bool{}, "", "main", time.Minute, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.account != "" {
		t.Errorf("account = %q, want empty", s.account)
	}
}

func TestIsEmpty(t *testing.T) {
	if !isEmpty(config{}) {
		t.Error("a zero config is empty")
	}
	for _, c := range []config{
		{Repo: "a/b"}, {Base: "main"}, {Interval: "1m"}, {Account: "alice"}, {Bots: []string{"x"}},
	} {
		if isEmpty(c) {
			t.Errorf("config %+v is not empty", c)
		}
	}
}

func TestSampleConfigIsValid(t *testing.T) {
	// The sample is what -print-config emits, so it must actually parse.
	path := writeConfig(t, sampleConfig)

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the sample config must parse: %v", err)
	}
	if cfg.Repo == "" || cfg.Base == "" || cfg.Interval == "" || len(cfg.Bots) == 0 {
		t.Errorf("the sample should demonstrate every field: %+v", cfg)
	}
	if _, err := resolve(cfg, map[string]bool{}, "", "main", time.Minute, ""); err != nil {
		t.Errorf("the sample config must resolve: %v", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	got := defaultConfigPath()
	if got == "" {
		t.Skip("no home directory in this environment")
	}
	if !strings.HasSuffix(got, filepath.Join(".config", "mqw", "config.toml")) {
		t.Errorf("defaultConfigPath() = %q", got)
	}
}
