package integrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// HooksFile returns the file that holds harness h's user-level hooks:
// $CLAUDE_CONFIG_DIR/settings.json or ~/.claude/settings.json for Claude
// Code, $CODEX_HOME/hooks.json or ~/.codex/hooks.json for Codex.
func HooksFile(h Harness, env Env) string {
	switch h {
	case HarnessCodex:
		if d := env.get("CODEX_HOME"); d != "" {
			return filepath.Join(d, "hooks.json")
		}
		return filepath.Join(homeDir(env), ".codex", "hooks.json")
	default:
		if d := env.get("CLAUDE_CONFIG_DIR"); d != "" {
			return filepath.Join(d, "settings.json")
		}
		return filepath.Join(homeDir(env), ".claude", "settings.json")
	}
}

// paceHookRE matches a whole hook command pace installed: one shell word (the
// pace binary, under any name or path, possibly quoted) followed by
// "hook <harness>-<event>" and nothing else but --quiet. Commands that only
// contain such a call among other things are not pace's.
var paceHookRE = regexp.MustCompile(`^\s*(?:'[^']*'|"(?:[^"\\]|\\.)*"|\\.|[^\s'"\\])+\s+hook\s+(claude|codex)-(session-start|pre-tool-use)(\s+--quiet)?\s*$`)

// IsPaceHook reports whether a hook command matches pace's hook syntax.
// The executable can have a custom name or path.
func IsPaceHook(command string) bool { return paceHookRE.MatchString(command) }

// object is a JSON object that keeps its key order; values stay raw.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(data []byte) (*object, error) {
	o := &object{vals: map[string]json.RawMessage{}}
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	return o, nil
}

func (o *object) get(k string) (json.RawMessage, bool) { v, ok := o.vals[k]; return v, ok }

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// rawOf marshals v without HTML escaping, so that commands such as
// ">/dev/null 2>&1" in other tools' hooks stay readable.
func rawOf(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// stripPaceGroups removes pace's hooks from an event's group list. Groups
// left without hooks by that are dropped; other groups stay byte-identical.
func stripPaceGroups(groups []json.RawMessage) (kept []json.RawMessage, removed int, err error) {
	for _, g := range groups {
		obj, err := parseObject(g)
		if err != nil {
			kept = append(kept, g) // not ours to judge
			continue
		}
		raw, ok := obj.get("hooks")
		if !ok {
			kept = append(kept, g)
			continue
		}
		var hooks []json.RawMessage
		if json.Unmarshal(raw, &hooks) != nil {
			kept = append(kept, g)
			continue
		}
		var keep []json.RawMessage
		for _, h := range hooks {
			var hc struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(h, &hc) == nil && IsPaceHook(hc.Command) {
				removed++
				continue
			}
			keep = append(keep, h)
		}
		switch {
		case len(keep) == len(hooks):
			kept = append(kept, g)
		case len(keep) == 0:
			// only pace hooks: drop the group
		default:
			obj.set("hooks", rawOf(keep))
			b, _ := obj.MarshalJSON()
			kept = append(kept, b)
		}
	}
	return kept, removed, nil
}

// ApplyHooks returns data (a Claude Code settings.json or a Codex
// hooks.json) with pace's hooks for harness h installed for paceBin, or with
// every pace hook removed when remove is true. Earlier pace hooks (any path,
// either harness name) are replaced, so applying is idempotent. Every other
// key, hook group and hook is kept, in order. changed reports whether the
// result differs semantically from data.
func ApplyHooks(data []byte, h Harness, paceBin string, remove bool) (out []byte, changed bool, err error) {
	root, err := parseObject(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse: %w", err)
	}
	hooks := &object{vals: map[string]json.RawMessage{}}
	if raw, ok := root.get("hooks"); ok {
		if hooks, err = parseObject(raw); err != nil {
			return nil, false, fmt.Errorf("parse \"hooks\": %w", err)
		}
	}
	before, _ := hooks.MarshalJSON()
	removedAny := false
	for _, ev := range append([]string(nil), hooks.keys...) {
		var groups []json.RawMessage
		if json.Unmarshal(hooks.vals[ev], &groups) != nil {
			continue
		}
		kept, n, _ := stripPaceGroups(groups)
		if n == 0 {
			continue
		}
		removedAny = true
		if len(kept) == 0 {
			hooks.del(ev)
		} else {
			hooks.set(ev, rawOf(kept))
		}
	}
	if !remove {
		add := PaceHooks(h, paceBin)
		for _, ev := range []string{"SessionStart", "PreToolUse"} {
			var groups []json.RawMessage
			if raw, ok := hooks.get(ev); ok {
				if err := json.Unmarshal(raw, &groups); err != nil {
					return nil, false, fmt.Errorf("\"hooks\".%s is not a list: %w", ev, err)
				}
			}
			for _, g := range add[ev] {
				groups = append(groups, rawOf(g))
			}
			hooks.set(ev, rawOf(groups))
		}
	}
	after, _ := hooks.MarshalJSON()
	changed = !jsonEqual(before, after)
	if !changed {
		return data, false, nil
	}
	if len(hooks.keys) == 0 && remove && removedAny {
		root.del("hooks")
	} else {
		root.set("hooks", after)
	}
	raw, _ := root.MarshalJSON()
	var compact, pretty bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, false, err
	}
	if err := json.Indent(&pretty, compact.Bytes(), "", "  "); err != nil {
		return nil, false, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), true, nil
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// HooksResult describes what UpdateHooksFile did.
type HooksResult struct {
	File    string
	Changed bool
	Backup  string // copy of the previous file ("" when none was made)
}

// UpdateHooksFile applies ApplyHooks to path in place: it writes a
// timestamped backup of the previous content next to it, then replaces the
// file atomically with the same permissions. A missing file is created
// (0600) unless remove is true. Unchanged content is not rewritten.
//
// A symlinked file (a dotfiles setup) is updated where the link points, and
// the link is kept.
func UpdateHooksFile(path string, h Harness, paceBin string, remove bool, now time.Time) (HooksResult, error) {
	res := HooksResult{File: path}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return res, fmt.Errorf("%s is a symlink that does not resolve: %w", path, err)
		}
		path = target
		res.File = target
	}
	data, err := os.ReadFile(path)
	mode := fs.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if remove {
			return res, nil
		}
		data = nil
	case err != nil:
		return res, err
	default:
		if st, err := os.Stat(path); err == nil {
			mode = st.Mode().Perm()
		}
	}
	out, changed, err := ApplyHooks(data, h, paceBin, remove)
	if err != nil {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return res, err
	}
	if data != nil {
		base := path + ".pace-backup-" + now.UTC().Format("20060102T150405Z")
		for n := 0; ; n++ {
			res.Backup = base
			if n > 0 {
				res.Backup = fmt.Sprintf("%s-%d", base, n)
			}
			f, err := os.OpenFile(res.Backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if errors.Is(err, fs.ErrExist) && n < 100 {
				continue
			}
			if err != nil {
				return res, fmt.Errorf("backup: %w", err)
			}
			_, werr := f.Write(data)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				return res, fmt.Errorf("backup: %w", werr)
			}
			break
		}
	}
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, "."+base+".pace-tmp*")
	if err != nil {
		return res, err
	}
	name := tmp.Name()
	_, werr := tmp.Write(out)
	if werr == nil {
		werr = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
		return res, werr
	}
	res.Changed = true
	return res, nil
}

// HooksInstalled reports which pace hook commands path holds, by event.
func HooksInstalled(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	found := map[string][]string{}
	for ev, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if IsPaceHook(h.Command) {
					found[ev] = append(found[ev], strings.TrimSpace(h.Command))
				}
			}
		}
	}
	return found, nil
}
