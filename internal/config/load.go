package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// getenvFunc abstracts os.Getenv so tests can resolve against a fake
// environment without mutating the process environment.
type getenvFunc func(string) string

var osEnv getenvFunc = os.Getenv

// userHome returns $HOME (from env), falling back to os.UserHomeDir.
func userHome(env getenvFunc) (string, error) {
	if h := env("HOME"); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", fmt.Errorf("config: cannot determine home directory: set HOME")
	}
	return h, nil
}

// DefaultPath is the config file location used when no path is given and
// $PACE_CONFIG is unset: $XDG_CONFIG_HOME/pace/config.toml, else
// ~/.config/pace/config.toml.
func DefaultPath() (string, error) { return defaultPath(osEnv) }

func defaultPath(env getenvFunc) (string, error) {
	if x := env("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "pace", "config.toml"), nil
	}
	h, err := userHome(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "pace", "config.toml"), nil
}

func load(path string, env getenvFunc) (Config, string, error) {
	explicit := true
	if path == "" {
		path = env("PACE_CONFIG")
	}
	if path == "" {
		explicit = false
		p, err := defaultPath(env)
		if err != nil {
			return Default(), "defaults", err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return Default(), "defaults", nil
		}
		return Default(), path, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(string(data))
	if err != nil {
		return Default(), path, fmt.Errorf("config %s: %w", path, err)
	}
	return c, path, nil
}

// Parse decodes TOML text strictly: unknown keys are an error naming every
// one of them (sorted).
func Parse(text string) (Config, error) {
	var c Config
	md, err := toml.Decode(text, &c)
	if err != nil {
		return Config{}, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, 0, len(und))
		for _, k := range und {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	return c, nil
}
