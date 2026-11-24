// Package integrate connects pace to the rest of the machine: launchd and
// systemd supervision, PATH shims that route heavy commands through
// `pace run`, Claude Code hook handlers, and a small client for the daemon's
// unix-socket API.
//
// Nothing here runs on the daemon's hot path. Everything that touches the
// system (launchctl, systemctl, files) goes through injectable seams so the
// tests never modify the real machine.
package integrate

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Class names the heavy table assigns. They match the default classes of the
// config package; an unknown class falls back to the default class there.
const (
	ClassBuild   = "build"
	ClassTest    = "test"
	ClassBrowser = "browser"
	ClassDefault = "default"
)

// tool describes how to classify one argv0.
type tool struct {
	// valueFlags are global flags (before the subcommand) that consume the
	// next argument, e.g. `go -C dir build`.
	valueFlags []string
	// subs maps a subcommand to a class. Missing = not heavy.
	subs map[string]string
	// any is the class when the tool is heavy whatever the subcommand
	// ("" = only listed subs are heavy).
	any string
	// bare is the class when there is no subcommand at all ("" = not heavy).
	bare string
	// classify, if set, overrides the table for this tool.
	classify func(args []string) (string, bool)
}

var nodePM = tool{
	valueFlags: []string{"--prefix", "-C", "--dir", "--filter", "-F", "--workspace", "-w", "--cwd"},
	subs: map[string]string{
		"install": ClassBuild, "i": ClassBuild, "add": ClassBuild, "ci": ClassBuild,
		"install-test": ClassTest, "it": ClassTest,
		"test": ClassTest, "t": ClassTest, "tst": ClassTest,
		"build": ClassBuild, "rebuild": ClassBuild,
	},
}

// tools is the heavy table keyed on the normalised argv0.
var tools = map[string]tool{
	"go": {
		valueFlags: []string{"-C"},
		subs: map[string]string{
			"build": ClassBuild, "run": ClassBuild, "vet": ClassBuild, "install": ClassBuild,
			"test": ClassTest,
		},
	},
	"cargo": {
		valueFlags: []string{"-C", "--config", "-Z", "--color"},
		subs: map[string]string{
			"build": ClassBuild, "b": ClassBuild, "run": ClassBuild, "r": ClassBuild,
			"clippy": ClassBuild, "check": ClassBuild, "c": ClassBuild, "install": ClassBuild,
			"test": ClassTest, "t": ClassTest, "bench": ClassTest, "nextest": ClassTest,
		},
	},
	"npm":  nodePM,
	"pnpm": nodePM,
	"yarn": withBare(nodePM, ClassBuild), // bare `yarn` installs
	"bun":  nodePM,
	"npx":  {classify: classifyNpx},
	"pnpx": {classify: classifyNpx},
	"bunx": {classify: classifyNpx},
	"xcodebuild": {classify: func(args []string) (string, bool) {
		for _, a := range args {
			switch a {
			case "-version", "-showsdks", "-list", "-help", "-usage", "-showBuildSettings",
				"-showdestinations", "-showTestPlans", "-license", "-checkFirstLaunchStatus":
				return "", false
			}
		}
		for _, a := range args {
			if a == "test" || a == "test-without-building" {
				return ClassTest, true
			}
		}
		return ClassBuild, true
	}},
	"swift":  {subs: map[string]string{"build": ClassBuild, "run": ClassBuild, "test": ClassTest}},
	"pytest": {any: ClassTest, bare: ClassTest},
	"python": {classify: classifyPython},
	"uv":     {classify: classifyUV},
	"make":   {classify: classifyMake},
	"gmake":  {classify: classifyMake},
	"tsc":    {any: ClassBuild, bare: ClassBuild},
	"docker": {classify: classifyDocker},
	"gradle": {any: ClassBuild, bare: ClassBuild, subs: map[string]string{"test": ClassTest, "check": ClassTest}},
	"mvn":    {any: ClassBuild, bare: ClassBuild, subs: map[string]string{"test": ClassTest, "verify": ClassTest}},
}

// classifyInner breaks the init cycle tools -> classifyUV -> Classify -> tools.
var classifyInner func([]string) (string, bool)

func init() { classifyInner = Classify }

func withBare(t tool, bare string) tool { t.bare = bare; return t }

// npxTools: the packages run via npx/pnpx/bunx/`pnpm exec` that are heavy.
var npxTools = map[string]string{
	"playwright": ClassBrowser, "@playwright/test": ClassBrowser,
	"vitest": ClassTest, "jest": ClassTest, "mocha": ClassTest,
	"tsc": ClassBuild, "next": ClassBuild, "vite": ClassBuild, "webpack": ClassBuild,
	"esbuild": ClassBuild, "turbo": ClassBuild, "nx": ClassBuild,
}

// HeavyTools lists every argv0 the heavy table knows, sorted. It is the
// default tool set for `pace shims install`.
func HeavyTools() []string {
	return []string{
		"bun", "bunx", "cargo", "docker", "go", "gradle", "make", "mvn", "npm", "npx",
		"pnpm", "pnpx", "pytest", "python", "python3", "swift", "tsc", "uv", "xcodebuild", "yarn",
	}
}

var pythonRE = regexp.MustCompile(`^python[0-9.]*$`)

// normalise maps an argv0 to its table key: basename, no .exe, and
// python3.12 -> python.
func normalise(argv0 string) string {
	b := filepath.Base(argv0)
	b = strings.TrimSuffix(b, ".exe")
	if pythonRE.MatchString(b) {
		return "python"
	}
	return b
}

// infoArgs make any tool invocation light (`tsc --version`, `make --help`).
var infoArgs = map[string]bool{
	"--version": true, "-version": true, "-V": true, "version": true,
	"--help": true, "-h": true, "help": true, "-help": true,
}

// Classify reports whether argv is a heavy command and which class it
// belongs to. argv[0] may be a bare name or a path. The decision only
// depends on argv; it never reads the environment or the filesystem.
func Classify(argv []string) (class string, heavy bool) {
	if len(argv) == 0 {
		return "", false
	}
	t, ok := tools[normalise(argv[0])]
	if !ok {
		return "", false
	}
	args := argv[1:]
	if len(args) > 0 && infoArgs[args[0]] {
		return "", false
	}
	if t.classify != nil {
		return t.classify(args)
	}
	sub, _ := subcommand(args, t.valueFlags)
	if sub == "" {
		if t.bare != "" {
			return t.bare, true
		}
		return "", false
	}
	if infoArgs[sub] {
		return "", false
	}
	if c, ok := t.subs[sub]; ok {
		return c, true
	}
	if isNodePM(t) {
		switch sub {
		case "run", "run-script", "rum", "urn":
			return classifyScript(args, t.valueFlags), true
		case "exec", "dlx", "x":
			_, rest := subcommand(args, t.valueFlags)
			if c, ok := classifyNpx(rest); ok {
				return c, true
			}
			return ClassBuild, true
		}
		// `pnpm vitest`, `yarn playwright test`: package managers run bins directly.
		if c, ok := classifyNpx([]string{sub}); ok {
			return c, true
		}
	}
	if t.any != "" {
		return t.any, true
	}
	return "", false
}

func isNodePM(t tool) bool { return t.subs["ci"] == ClassBuild && t.subs["it"] == ClassTest }

// classifyScript classifies `npm run <script>`: test* -> test,
// e2e/playwright -> browser, anything else -> build.
func classifyScript(args, valueFlags []string) string {
	_, rest := subcommand(args, valueFlags)
	script, _ := subcommand(rest, nil)
	s := strings.ToLower(script)
	switch {
	case strings.Contains(s, "e2e") || strings.Contains(s, "playwright"):
		return ClassBrowser
	case strings.HasPrefix(s, "test") || strings.Contains(s, ":test") || strings.HasSuffix(s, "test"):
		return ClassTest
	default:
		return ClassBuild
	}
}

// subcommand returns the first non-flag argument (skipping the values of
// valueFlags) and the arguments after it.
func subcommand(args, valueFlags []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1], args[i+2:]
			}
			return "", nil
		}
		if strings.HasPrefix(a, "+") { // cargo +nightly
			continue
		}
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && contains(valueFlags, a) {
				i++
			}
			continue
		}
		return a, args[i+1:]
	}
	return "", nil
}

// stripGlobalFlags drops leading global flags (and their values) so that a
// custom classifier sees the subcommand first.
func stripGlobalFlags(args, valueFlags []string) []string {
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			return args[i+1:]
		}
		if strings.HasPrefix(a, "+") {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		if !strings.Contains(a, "=") && contains(valueFlags, a) {
			i++
		}
		i++
	}
	if i > len(args) {
		return nil
	}
	return args[i:]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func classifyNpx(args []string) (string, bool) {
	vf := []string{"-p", "--package", "-c", "--call", "--node-options"}
	pkg, _ := subcommand(args, vf)
	if pkg == "" {
		return "", false
	}
	name := pkg
	// strip a version suffix: vitest@1.2 / @playwright/test@1.40
	if at := strings.LastIndexByte(name, '@'); at > 0 {
		name = name[:at]
	}
	c, ok := npxTools[name]
	return c, ok
}

func classifyPython(args []string) (string, bool) {
	vf := []string{"-X", "-W", "-c"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" && i+1 < len(args):
			if args[i+1] == "pytest" {
				return ClassTest, true
			}
			return "", false
		case strings.HasPrefix(a, "-m") && len(a) > 2:
			if a[2:] == "pytest" {
				return ClassTest, true
			}
			return "", false
		case a == "-c":
			return "", false
		case strings.HasPrefix(a, "-"):
			if contains(vf, a) {
				i++
			}
		default:
			return "", false // a script: not classified
		}
	}
	return "", false
}

func classifyUV(args []string) (string, bool) {
	args = stripGlobalFlags(args, []string{"--directory", "--project", "--python", "--config-file", "--cache-dir", "--color"})
	if len(args) == 0 {
		return "", false
	}
	switch args[0] {
	case "run":
		// `uv run [flags] pytest ...`: classify the inner command if we can.
		inner := stripGlobalFlags(args[1:], []string{"--with", "--python", "-p", "--directory", "--project", "--package", "--extra", "--group", "--env-file"})
		if c, ok := classifyInner(inner); ok {
			return c, true
		}
		return ClassDefault, true
	case "sync", "pip", "lock", "build":
		return ClassBuild, true
	case "tool":
		if len(args) > 1 && (args[1] == "run" || args[1] == "install") {
			return ClassBuild, true
		}
	}
	return "", false
}

func classifyMake(args []string) (string, bool) {
	for _, a := range args {
		if a == "-n" || a == "--dry-run" || a == "--just-print" || a == "-q" || a == "--question" {
			return "", false
		}
	}
	makeValueFlags := []string{"-C", "-f", "--file", "--directory", "-j", "--jobs", "-l", "--load-average", "-I", "--include-dir", "-o", "-W"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if contains(makeValueFlags, a) && i+1 < len(args) && (a != "-j" && a != "--jobs" && a != "-l" || isNumber(args[i+1])) {
				i++ // -j takes an optional numeric value
			}
			continue
		}
		if strings.Contains(a, "=") {
			continue // VAR=value
		}
		if a == "test" || a == "check" || strings.HasPrefix(a, "test") {
			return ClassTest, true
		}
	}
	return ClassBuild, true
}

func classifyDocker(args []string) (string, bool) {
	args = stripGlobalFlags(args, []string{"--context", "-c", "-H", "--host", "--config", "-l", "--log-level"})
	if len(args) == 0 {
		return "", false
	}
	switch args[0] {
	case "build":
		return ClassBuild, true
	case "buildx":
		if sub, _ := subcommand(args[1:], nil); sub == "build" || sub == "bake" {
			return ClassBuild, true
		}
	case "compose":
		sub, _ := subcommand(args[1:], []string{"-f", "--file", "-p", "--project-name", "--profile", "--env-file", "--project-directory"})
		switch sub {
		case "up", "build", "run", "create":
			return ClassBuild, true
		}
	}
	return "", false
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
