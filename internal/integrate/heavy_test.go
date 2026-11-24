package integrate

import (
	"sort"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd   string
		class string // "" = not heavy
	}{
		// go
		{"go build ./...", ClassBuild},
		{"go test -race ./...", ClassTest},
		{"go run ./cmd/x", ClassBuild},
		{"go vet ./...", ClassBuild},
		{"go install ./cmd/pace", ClassBuild},
		{"go -C sub build", ClassBuild},
		{"go -C=sub test", ClassTest},
		{"go version", ""},
		{"go env GOPATH", ""},
		{"go mod tidy", ""},
		{"go fmt ./...", ""},
		{"go", ""},
		{"go --help", ""},
		{"/usr/local/go/bin/go build", ClassBuild},
		// cargo
		{"cargo build --release", ClassBuild},
		{"cargo +nightly build", ClassBuild},
		{"cargo test", ClassTest},
		{"cargo t", ClassTest},
		{"cargo run", ClassBuild},
		{"cargo clippy", ClassBuild},
		{"cargo check", ClassBuild},
		{"cargo nextest run", ClassTest},
		{"cargo fmt", ""},
		{"cargo --version", ""},
		{"cargo -C dir build", ClassBuild},
		// node package managers
		{"npm install", ClassBuild},
		{"npm i", ClassBuild},
		{"npm ci", ClassBuild},
		{"npm test", ClassTest},
		{"npm t", ClassTest},
		{"npm run build", ClassBuild},
		{"npm run test:unit", ClassTest},
		{"npm run unit:test", ClassTest},
		{"npm run e2e", ClassBrowser},
		{"npm run lint", ClassBuild},
		{"npm --prefix web install", ClassBuild},
		{"npm exec vitest", ClassTest},
		{"npm exec some-tool", ClassBuild},
		{"npm view react", ""},
		{"npm --version", ""},
		{"npm", ""},
		{"pnpm install", ClassBuild},
		{"pnpm --filter web build", ClassBuild},
		{"pnpm -r test", ClassTest},
		{"pnpm exec playwright test", ClassBrowser},
		{"pnpm dlx vite build", ClassBuild},
		{"pnpm vitest run", ClassTest},
		{"pnpm outdated", ""},
		{"yarn", ClassBuild},
		{"yarn install", ClassBuild},
		{"yarn test", ClassTest},
		{"yarn jest", ClassTest},
		{"yarn why react", ""},
		{"bun install", ClassBuild},
		{"bun test", ClassTest},
		{"bun run build", ClassBuild},
		{"bun x vitest", ClassTest},
		// npx family
		{"npx playwright test", ClassBrowser},
		{"npx -y @playwright/test test", ClassBrowser},
		{"npx vitest", ClassTest},
		{"npx vitest@1.2.0 run", ClassTest},
		{"npx jest --ci", ClassTest},
		{"npx tsc -p .", ClassBuild},
		{"npx next build", ClassBuild},
		{"npx vite", ClassBuild},
		{"npx -p typescript tsc", ClassBuild}, // -p value skipped, then tsc
		{"npx cowsay hi", ""},
		{"npx", ""},
		{"pnpx vitest", ClassTest},
		{"bunx playwright test", ClassBrowser},
		// xcode / swift
		{"xcodebuild -scheme App build", ClassBuild},
		{"xcodebuild -scheme App test", ClassTest},
		{"xcodebuild -version", ""},
		{"xcodebuild -list", ""},
		{"xcodebuild", ClassBuild},
		{"swift build", ClassBuild},
		{"swift test", ClassTest},
		{"swift run", ClassBuild},
		{"swift package resolve", ""},
		{"swift", ""},
		// python
		{"pytest", ClassTest},
		{"pytest -x tests/", ClassTest},
		{"pytest --version", ""},
		{"python -m pytest -q", ClassTest},
		{"python3 -m pytest", ClassTest},
		{"python3.12 -m pytest", ClassTest},
		{"python -mpytest", ClassTest},
		{"python -u -m pytest", ClassTest},
		{"python -X dev -m pytest", ClassTest},
		{"python -m http.server", ""},
		{"python script.py", ""},
		{"python -c 'print(1)'", ""},
		{"python", ""},
		{"uv run pytest", ClassTest},
		{"uv run --with x pytest -q", ClassTest},
		{"uv run python app.py", ClassDefault},
		{"uv run", ClassDefault},
		{"uv sync", ClassBuild},
		{"uv pip install x", ClassBuild},
		{"uv --directory sub sync", ClassBuild},
		{"uv tool run ruff", ClassBuild},
		{"uv venv", ""},
		{"uv --version", ""},
		// make
		{"make", ClassBuild},
		{"make -j8", ClassBuild},
		{"make -C sub all", ClassBuild},
		{"make test", ClassTest},
		{"make -j 4 check", ClassTest},
		{"make CC=clang", ClassBuild},
		{"make -j test", ClassTest},      // -j takes an optional numeric value
		{"make -j 8 all", ClassBuild},    // numeric value consumed
		{"make -C test all", ClassBuild}, // "test" is the -C directory
		{"make -n", ""},
		{"make --help", ""},
		{"gmake", ClassBuild},
		// tsc
		{"tsc", ClassBuild},
		{"tsc -p .", ClassBuild},
		{"tsc --version", ""},
		// docker
		{"docker build .", ClassBuild},
		{"docker --context remote build .", ClassBuild},
		{"docker buildx build .", ClassBuild},
		{"docker compose up -d", ClassBuild},
		{"docker compose -f x.yml build", ClassBuild},
		{"docker compose ps", ""},
		{"docker ps", ""},
		{"docker", ""},
		// gradle / mvn
		{"gradle build", ClassBuild},
		{"gradle test", ClassTest},
		{"gradle", ClassBuild},
		{"mvn package", ClassBuild},
		{"mvn test", ClassTest},
		{"mvn -v", ClassBuild}, // -v is not an info arg for mvn table (only --version/-V)
		{"mvn --version", ""},
		// not heavy
		{"ls -la", ""},
		{"git status", ""},
		{"clang -c x.c", ""},
		{"gcc -j4", ""},
		{"", ""},
	}
	for _, c := range cases {
		argv := strings.Fields(c.cmd)
		got, heavy := Classify(argv)
		if heavy != (c.class != "") || got != c.class {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, %v)", c.cmd, got, heavy, c.class, c.class != "")
		}
		// Deterministic: same answer twice.
		got2, heavy2 := Classify(argv)
		if got2 != got || heavy2 != heavy {
			t.Errorf("Classify(%q) not deterministic", c.cmd)
		}
	}
}

func TestClassifyNilAndEmpty(t *testing.T) {
	if c, h := Classify(nil); h || c != "" {
		t.Fatalf("nil argv: %q %v", c, h)
	}
	if c, h := Classify([]string{}); h || c != "" {
		t.Fatalf("empty argv: %q %v", c, h)
	}
	// A flag with a missing value must not panic.
	for _, argv := range [][]string{{"go", "-C"}, {"npm", "--prefix"}, {"npx", "-p"}, {"python", "-m"}, {"python", "-X"}, {"docker", "compose", "-f"}, {"make", "-C"}, {"uv", "run", "--with"}} {
		Classify(argv)
	}
}

func TestNormalise(t *testing.T) {
	cases := map[string]string{
		"go":                  "go",
		"/usr/bin/python3":    "python",
		"python3.12":          "python",
		"python":              "python",
		"pythonista":          "pythonista",
		"C:/x/cargo.exe":      "cargo",
		"/opt/homebrew/bin/x": "x",
	}
	for in, want := range cases {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeavyToolsCoverTable(t *testing.T) {
	list := HeavyTools()
	if !sort.StringsAreSorted(list) {
		t.Fatalf("HeavyTools not sorted: %v", list)
	}
	seen := map[string]bool{}
	for _, n := range list {
		if seen[n] {
			t.Fatalf("duplicate %q", n)
		}
		seen[n] = true
		if _, ok := tools[normalise(n)]; !ok {
			t.Errorf("HeavyTools lists %q which the table does not know", n)
		}
	}
	for k := range tools {
		if k == "gmake" {
			continue // gmake is rare; not shimmed by default
		}
		if !seen[k] {
			t.Errorf("table key %q missing from HeavyTools", k)
		}
	}
}

func TestSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		vf   []string
		sub  string
		rest int
	}{
		{[]string{"build", "x"}, nil, "build", 1},
		{[]string{"-v", "build"}, nil, "build", 0},
		{[]string{"-C", "dir", "build"}, []string{"-C"}, "build", 0},
		{[]string{"-C=dir", "build"}, []string{"-C"}, "build", 0},
		{[]string{"--", "build", "a", "b"}, nil, "build", 2},
		{[]string{"--"}, nil, "", 0},
		{[]string{"-x"}, nil, "", 0},
		{nil, nil, "", 0},
	}
	for _, c := range cases {
		sub, rest := subcommand(c.args, c.vf)
		if sub != c.sub || len(rest) != c.rest {
			t.Errorf("subcommand(%v) = %q %v", c.args, sub, rest)
		}
	}
}
