package integrate

import (
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/fireharp/pace/internal/config"
)

// uncomment turns "# key = value  # note" and "# [section]" lines into live
// TOML so every documented key is checked against the Config struct.
func uncomment(s string) string {
	live := regexp.MustCompile(`^# (\[[a-z_.]+\](?:\s+#.*)?|[a-z_]+ = .*)$`)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := live.FindStringSubmatch(line); m != nil {
			line = m[1]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestExampleConfigDecodesStrictly(t *testing.T) {
	for name, text := range map[string]string{"as shipped": ExampleConfig, "all uncommented": uncomment(ExampleConfig)} {
		var c config.Config
		md, err := toml.Decode(text, &c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if und := md.Undecoded(); len(und) > 0 {
			t.Fatalf("%s: unknown keys %v", name, und)
		}
		if c.Mode != "observe" {
			t.Fatalf("%s: mode %q", name, c.Mode)
		}
	}
	var c config.Config
	if _, err := toml.Decode(uncomment(ExampleConfig), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Classes) != 5 || c.Classes["build"].Budget != "4G,30%" || c.Enforce.Grace != "5s" || c.FailOpen.Slots != 2 {
		t.Fatalf("uncommented values not decoded: %+v", c)
	}
	if !strings.Contains(uncomment(ExampleConfig), "\n[classes.model]\n") {
		t.Fatal("uncomment did not activate sections")
	}
}
