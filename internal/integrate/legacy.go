package integrate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// pace was called gov before. These are the names gov installed under, so
// 'pace migrate' can find and remove what an old install left behind.
const (
	LegacyName     = "gov"
	LegacyLabel    = "com.gov.daemon"
	LegacyUnitName = "gov.service"
)

// LegacyPlistPath is ~/Library/LaunchAgents/com.gov.daemon.plist.
func LegacyPlistPath(home string) string { return plistPathFor(home, LegacyLabel) }

// LegacyUnitPath is $XDG_CONFIG_HOME/systemd/user/gov.service.
func LegacyUnitPath(home string, env Env) string { return unitPathFor(home, env, LegacyUnitName) }

// LegacyServiceFile is the service file gov installed on goos.
func LegacyServiceFile(goos, home string, env Env) string {
	if goos == "darwin" {
		return LegacyPlistPath(home)
	}
	return LegacyUnitPath(home, env)
}

// LegacyShimDir is gov's default shim directory, ~/.gov/shims.
func LegacyShimDir(env Env) string { return filepath.Join(homeDir(env), ".gov", "shims") }

// ConfigDir is the default config directory: $XDG_CONFIG_HOME/pace, else
// ~/.config/pace. LegacyConfigDir is the same for gov.
func ConfigDir(env Env) string { return configDirFor(env, "pace") }

// LegacyConfigDir is $XDG_CONFIG_HOME/gov, else ~/.config/gov.
func LegacyConfigDir(env Env) string { return configDirFor(env, LegacyName) }

func configDirFor(env Env, name string) string {
	if x := env.get("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, name)
	}
	return filepath.Join(homeDir(env), ".config", name)
}

var (
	plistEnvRE = regexp.MustCompile(`<key>(?:GOV|PACE)_(MODE|CONFIG|HOME|SOCK)</key>\s*<string>([^<]*)</string>`)
	unitEnvRE  = regexp.MustCompile(`(?m)^Environment="?(?:GOV|PACE)_(MODE|CONFIG|HOME|SOCK)=((?:[^"\\\n]|\\.)*)"?\s*$`)
)

// ServiceFileEnv returns the MODE, CONFIG, HOME and SOCK settings a service
// file passes to the daemon, keyed without prefix ("MODE": "observe"). It
// reads both gov's (GOV_*) and pace's (PACE_*) names. A missing file is an
// empty map.
func ServiceFileEnv(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, m := range plistEnvRE.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = xmlUnescaper.Replace(m[2])
	}
	for _, m := range unitEnvRE.FindAllStringSubmatch(string(data), -1) {
		out[m[1]] = sdUnescaper.Replace(m[2])
	}
	return out
}

var (
	xmlUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#34;", `"`, "&apos;", "'", "&#39;", "'", "&amp;", "&")
	sdUnescaper  = strings.NewReplacer(`\\`, `\`, `\"`, `"`, "%%", "%", "$$", "$")
)
