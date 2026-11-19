package tote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote quotes a string for a POSIX shell: single quotes, with each embedded single quote closed, escaped and
// reopened. A string of only safe characters is returned as it is.
func ShellQuote(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InstallOptions are the settings baked into the hook command.
type InstallOptions struct {
	SkipIf string // TOTE_SKIP_IF
	Redact bool   // TOTE_REDACT=1
}

// HookCommand is the exact command line the installer writes for a binary.
func HookCommand(bin string, o InstallOptions) string {
	var b strings.Builder
	if o.SkipIf != "" {
		b.WriteString("TOTE_SKIP_IF=" + ShellQuote(o.SkipIf) + " ")
	}
	if o.Redact {
		b.WriteString("TOTE_REDACT=1 ")
	}
	return b.String() + ShellQuote(bin) + " hook"
}

var envPrefix = regexp.MustCompile(`^(?:TOTE_SKIP_IF=(?:'(?:[^']|'\\'')*'|[A-Za-z0-9_@%+=:,./-]+) )?(?:TOTE_REDACT=1 )?$`)

// toteBin matches a binary named tote at any path, quoted or bare, as HookCommand writes it.
var toteBin = regexp.MustCompile(`(?:'(?:[^']|'\\'')*/tote'|[A-Za-z0-9_@%+=:,./-]*/tote)$`)

// isOurs reports whether a command is one HookCommand wrote, for this binary or a tote binary at another path, with
// any of its env settings: a reinstall from a new path replaces the old hook instead of leaving it running beside it.
func isOurs(cmd, bin string) bool {
	rest, ok := strings.CutSuffix(cmd, " hook")
	if !ok {
		return false
	}
	if r, ok := strings.CutSuffix(rest, ShellQuote(bin)); ok {
		return envPrefix.MatchString(r)
	}
	loc := toteBin.FindStringIndex(rest)
	return loc != nil && envPrefix.MatchString(rest[:loc[0]])
}

// isLegacy matches the hook command of the Python version, which a fresh install replaces.
func isLegacy(cmd string) bool { return strings.Contains(cmd, "-m tote.check") }

// Install merges tote's Stop hook into PROJECT_DIR/.claude/settings.local.json, or takes it out. The other settings
// and hooks are kept, a backup is written before a change, and an existing tote hook is replaced rather than added
// twice. Key order inside the file may change, since the file is read and written as JSON.
func Install(projectDir, bin string, remove bool, o InstallOptions, w io.Writer) error {
	if !remove && o.SkipIf != "" {
		if _, err := regexp.Compile(o.SkipIf); err != nil {
			return fmt.Errorf("TOTE_SKIP_IF is not a valid pattern (Go regexp syntax, which has no lookahead or backreferences): %w", err)
		}
	}
	if strings.Contains(bin, "go-build") {
		return fmt.Errorf("%s looks like a temporary build; install the binary first (go install github.com/justinstimatze/tote@latest)", bin)
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	dest := filepath.Join(root, ".claude", "settings.local.json")
	if r, err := filepath.EvalSymlinks(dest); err == nil {
		dest = r // write through a symlinked settings file instead of replacing the link
	}
	settings := map[string]any{}
	mode := os.FileMode(0o644)
	var original []byte
	if b, err := os.ReadFile(dest); err == nil {
		original = b
		if st, err := os.Stat(dest); err == nil {
			mode = st.Mode().Perm()
		}
		if len(bytes.TrimSpace(b)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.UseNumber() // keep numbers as written
			if err := dec.Decode(&settings); err != nil {
				return fmt.Errorf("%s is not valid JSON, so it was left alone: %w", dest, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if settings["hooks"] != nil && !ok {
		return fmt.Errorf("%s: \"hooks\" is not an object, so it was left alone", dest)
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	stop, ok := hooks["Stop"].([]any)
	if hooks["Stop"] != nil && !ok {
		return fmt.Errorf("%s: hooks.Stop is not a list, so it was left alone", dest)
	}
	removed := 0
	var groups []any
	for _, g := range stop {
		gm, isMap := g.(map[string]any)
		hs, isList := gm["hooks"].([]any)
		if !isMap || !isList {
			groups = append(groups, g)
			continue
		}
		var keep []any
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if cmd, _ := hm["command"].(string); cmd != "" && (isOurs(cmd, bin) || (!remove && isLegacy(cmd))) {
				removed++
				continue
			}
			keep = append(keep, h)
		}
		if len(keep) == 0 {
			continue // the group held only tote's hook
		}
		gm["hooks"] = keep
		groups = append(groups, gm)
	}
	if remove {
		if removed == 0 {
			fmt.Fprintf(w, "no tote hook for %s found in %s\n", bin, dest)
			return nil
		}
	} else {
		groups = append(groups, map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": HookCommand(bin, o), "timeout": HookTimeout,
			// async: the user never waits on the check, and its result reaches the agent with the next message
			"async": true}}})
	}
	if len(groups) > 0 {
		hooks["Stop"] = groups
	} else {
		delete(hooks, "Stop")
	}
	if len(hooks) > 0 {
		settings["hooks"] = hooks
	} else {
		delete(settings, "hooks")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if original != nil {
		// one backup, replaced each run, created fresh so a planted symlink at that name is not followed
		bak := dest + ".bak"
		if err := os.Remove(bak); err != nil && !os.IsNotExist(err) {
			return err
		}
		f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.Write(original)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".settings-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if remove {
		fmt.Fprintf(w, "removed the tote Stop hook from %s\n", dest)
	} else {
		fmt.Fprintf(w, "wrote the tote Stop hook into %s\n", dest)
	}
	return nil
}
