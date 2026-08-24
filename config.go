package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// localConfigName is the per-directory config, found by walking up from the
// working directory. It overrides the global file field by field.
const localConfigName = ".mqw.toml"

// config is the on-disk settings file. Every field is optional; a later layer
// overrides an earlier one only where it says something.
type config struct {
	Repo     string   `toml:"repo"`
	Base     string   `toml:"base"`
	Interval string   `toml:"interval"`
	Account  string   `toml:"account"`
	Bots     []string `toml:"bots"`
}

const sampleConfig = `# Global config: ~/.config/mqw/config.toml
# Per-repository override: .mqw.toml beside the checkout (found by walking up).
# A flag beats both.

repo = "acme/service"
base = "main"
interval = "1m"

# Pin gh to one account. gh prefers GH_TOKEN over whichever account is active,
# so this keeps mqw working regardless of 'gh auth switch' run in another
# terminal. Leave unset to use the active account.
account = "alice"

# Logins to treat as bots. GitHub Apps are detected automatically; list the
# service accounts that are ordinary Users here, since nothing marks those.
bots = ["deps-bot", "release-bot"]
`

// defaultConfigPath is ~/.config/mqw/config.toml. An unresolvable home directory
// yields "", which loadConfig treats as no config.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mqw", "config.toml")
}

// loadConfig reads one settings file. A missing file is not an error, so the tool
// works with no config at all; malformed TOML is, since silently ignoring it
// would leave the user wondering why their settings do nothing.
func loadConfig(path string) (config, error) {
	var cfg config
	if path == "" {
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config{}, nil
		}
		return config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return cfg, nil
}

// findLocalConfig walks up from dir looking for .mqw.toml, stopping once it has
// examined the directory holding .git. Returns "" when there is none.
func findLocalConfig(dir string) string {
	if dir == "" {
		return ""
	}
	for {
		candidate := filepath.Join(dir, localConfigName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		// The repository root is the last place worth looking: past it we would
		// wander into unrelated parents.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// merge overlays one config on another. A field only overrides where the override
// actually sets it, so a per-repo file can pin the account without restating the
// repo, base or interval.
//
// A non-empty bots list replaces rather than appends: predictable beats clever
// when the question is "why is this PR showing as a bot".
func merge(base, override config) config {
	out := base
	if override.Repo != "" {
		out.Repo = override.Repo
	}
	if override.Base != "" {
		out.Base = override.Base
	}
	if override.Interval != "" {
		out.Interval = override.Interval
	}
	if override.Account != "" {
		out.Account = override.Account
	}
	if len(override.Bots) > 0 {
		out.Bots = override.Bots
	}
	return out
}

// loadConfigs reads the global file then the nearest per-repository one, and
// returns the merged result plus the files that contributed, in precedence order.
func loadConfigs(globalPath, startDir string) (config, []string, error) {
	var sources []string

	global, err := loadConfig(globalPath)
	if err != nil {
		return config{}, nil, err
	}
	if !isEmpty(global) {
		sources = append(sources, globalPath)
	}

	localPath := findLocalConfig(startDir)
	if localPath == "" {
		return global, sources, nil
	}
	local, err := loadConfig(localPath)
	if err != nil {
		return config{}, nil, err
	}
	sources = append(sources, localPath)
	return merge(global, local), sources, nil
}

// isEmpty reports whether a config said nothing at all. Written out field by
// field because a struct holding a slice is not comparable with ==.
func isEmpty(c config) bool {
	return c.Repo == "" && c.Base == "" && c.Interval == "" && c.Account == "" && len(c.Bots) == 0
}

// settings is the resolved configuration the program runs on.
type settings struct {
	owner, name string
	base        string
	interval    time.Duration
	account     string
	bots        map[string]bool
	sources     []string
}

// resolve merges the config with the flags, where a flag that was actually passed
// always wins. setFlags names the flags the user gave on the command line.
func resolve(cfg config, setFlags map[string]bool, repo, base string, interval time.Duration, account string) (settings, error) {
	var s settings

	if !setFlags["repo"] && cfg.Repo != "" {
		repo = cfg.Repo
	}
	owner, name, err := parseRepo(repo)
	if err != nil {
		return s, err
	}

	if !setFlags["base"] && cfg.Base != "" {
		base = cfg.Base
	}
	if !setFlags["account"] && cfg.Account != "" {
		account = cfg.Account
	}
	if !setFlags["interval"] && cfg.Interval != "" {
		d, err := time.ParseDuration(cfg.Interval)
		if err != nil {
			return s, fmt.Errorf("config interval %q: %w", cfg.Interval, err)
		}
		interval = d
	}
	if interval <= 0 {
		return s, fmt.Errorf("interval must be positive, got %s", interval)
	}

	bots := make(map[string]bool, len(cfg.Bots))
	for _, login := range cfg.Bots {
		if login != "" {
			bots[login] = true
		}
	}

	return settings{
		owner:    owner,
		name:     name,
		base:     base,
		interval: interval,
		account:  account,
		bots:     bots,
	}, nil
}
