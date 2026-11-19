package tote

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func settingsOf(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func stopCommands(s map[string]any) []string {
	var out []string
	hooks, _ := s["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	for _, g := range stop {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

func TestShellQuoteRoundTripsThroughAShell(t *testing.T) {
	for _, s := range []string{"/usr/local/bin/tote", "/home/a b/tote", "/home/#x/tote", "/home/a&b/to;te", "/home/it's/tote", `/home/"q"/tote`, "/home/$HOME/`x`/tote", "/home/\ttab/tote"} {
		out, err := exec.Command("sh", "-c", "printf %s "+ShellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q: shell gave %q (%v)", s, out, err)
		}
	}
}

func TestInstallMergesKeepsOtherSettingsAndReplacesRatherThanDuplicates(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"permissions":{"allow":["Bash(ls)"]},"big":12345678901234567890,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"other-hook"}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"x"}]}]}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.local.json"), []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}
	bin := "/opt/my tools/it's#tote"
	var out bytes.Buffer
	for i := 0; i < 2; i++ { // twice: the second replaces the first
		if err := Install(dir, bin, false, InstallOptions{}, &out); err != nil {
			t.Fatal(err)
		}
	}
	s := settingsOf(t, dir)
	cmds := stopCommands(s)
	want := HookCommand(bin, InstallOptions{})
	if len(cmds) != 2 || cmds[0] != "other-hook" || cmds[1] != want {
		t.Fatalf("commands: %q (want other-hook and %q)", cmds, want)
	}
	if _, ok := s["permissions"]; !ok {
		t.Fatal("permissions lost")
	}
	if hooks := s["hooks"].(map[string]any); hooks["PreToolUse"] == nil {
		t.Fatal("PreToolUse lost")
	}
	raw, _ := os.ReadFile(filepath.Join(cfgDir, "settings.local.json"))
	if !strings.Contains(string(raw), "12345678901234567890") || !strings.Contains(string(raw), `"timeout": 120`) || !strings.Contains(string(raw), `"async": true`) {
		t.Fatalf("number mangled or timeout missing: %s", raw)
	}
	baks, _ := filepath.Glob(filepath.Join(cfgDir, "settings.local.json.bak-*"))
	if len(baks) == 0 {
		t.Fatal("no backup written")
	}
	if st, _ := os.Stat(filepath.Join(cfgDir, "settings.local.json")); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode changed to %v", st.Mode().Perm())
	}
	// the command quotes the path so a shell reads it back as one word
	sh, err := exec.Command("sh", "-c", `set -- `+strings.TrimSuffix(want, " hook")+`; printf %s "$1"`).Output()
	if err != nil || string(sh) != bin {
		t.Fatalf("shell read %q (%v)", sh, err)
	}
}

func TestInstallRemoveTakesOutOnlyTheExactCommand(t *testing.T) {
	dir := t.TempDir()
	binA, binB := "/opt/a/tote", "/opt/b/tote"
	var out bytes.Buffer
	for _, b := range []string{binA, binB} {
		if err := Install(dir, b, false, InstallOptions{}, &out); err != nil {
			t.Fatal(err)
		}
	}
	// installing the second binary added a second hook: only the exact path is replaced
	if got := stopCommands(settingsOf(t, dir)); len(got) != 2 {
		t.Fatalf("%q", got)
	}
	if err := Install(dir, binA, true, InstallOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if got := stopCommands(settingsOf(t, dir)); len(got) != 1 || got[0] != HookCommand(binB, InstallOptions{}) {
		t.Fatalf("%q", got)
	}
	out.Reset()
	if err := Install(dir, binA, true, InstallOptions{}, &out); err != nil || !strings.Contains(out.String(), "no tote hook") {
		t.Fatalf("%v %q", err, out.String())
	}
	if err := Install(dir, binB, true, InstallOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if s := settingsOf(t, dir); s["hooks"] != nil {
		t.Fatalf("empty hooks left behind: %v", s)
	}
}

func TestInstallRemoveMatchesEnvPrefixButNotLookalikes(t *testing.T) {
	dir := t.TempDir()
	bin := "/opt/a/tote"
	var out bytes.Buffer
	o := InstallOptions{SkipIf: "prod's (db|users)", Redact: true}
	if err := Install(dir, bin, false, o, &out); err != nil {
		t.Fatal(err)
	}
	if got := stopCommands(settingsOf(t, dir)); len(got) != 1 || !strings.HasPrefix(got[0], "TOTE_SKIP_IF='prod'\\''s (db|users)' TOTE_REDACT=1 ") {
		t.Fatalf("%q", got)
	}
	// a different command that merely ends in the same words is not ours
	for _, cmd := range []string{"echo hi && " + bin + " hook", "sudo " + bin + " hook", bin + " hook --extra"} {
		if isOurs(cmd, bin) {
			t.Errorf("%q treated as ours", cmd)
		}
	}
	if err := Install(dir, bin, true, InstallOptions{}, &out); err != nil {
		t.Fatal(err)
	}
	if s := settingsOf(t, dir); s["hooks"] != nil {
		t.Fatalf("hook left behind: %v", s)
	}
}

func TestInstallReplacesTheLegacyPythonHook(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	legacy := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"uv run --quiet --project /x python -W ignore -m tote.check","timeout":300}]}]}}`
	_ = os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(legacy), 0o644)
	if err := Install(dir, "/opt/a/tote", false, InstallOptions{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := stopCommands(settingsOf(t, dir)); len(got) != 1 || got[0] != "/opt/a/tote hook" {
		t.Fatalf("%q", got)
	}
}

func TestInstallRejectsAnInvalidSkipPatternAndBrokenSettings(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir, "/opt/a/tote", false, InstallOptions{SkipIf: "(?=x)"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "TOTE_SKIP_IF") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); err == nil {
		t.Fatal("a rejected install wrote files")
	}
	_ = os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	p := filepath.Join(dir, ".claude", "settings.local.json")
	_ = os.WriteFile(p, []byte("{not json"), 0o644)
	if err := Install(dir, "/opt/a/tote", false, InstallOptions{}, &bytes.Buffer{}); err == nil {
		t.Fatal("broken settings accepted")
	}
	if b, _ := os.ReadFile(p); string(b) != "{not json" {
		t.Fatalf("broken settings overwritten: %s", b)
	}
}
