package tote

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MinChars is the reply length below which a reply is not checked: shorter replies rarely carry checkable claims.
const MinChars = 400

// Config is what the environment sets. TOTE_SKIP_IF and TOTE_REDACT are baked into the hook command by `tote
// install`; the rest can be set in Claude Code's environment.
type Config struct {
	Model, Effort string
	DailyUSD      float64 // the cap, when DailySet
	DailySet      bool
	Sample        float64
	Redact        bool
	SkipIfRaw     string
	SkipIf        *regexp.Regexp // compiled once; nil when unset or invalid
	SkipIfErr     error
	StateDir      string
	EnvFile       string // where the API key file is looked for
	Home          string
}

// LoadConfig reads the environment. A malformed number is returned as an error, so the hook can fail closed on it.
func LoadConfig() (*Config, error) {
	home, _ := os.UserHomeDir()
	c := &Config{
		Model:   envOr("TOTE_ASK_MODEL", "claude-sonnet-5-5"),
		Effort:  envOr("TOTE_ASK_EFFORT", "medium"),
		Sample:  1,
		Redact:  os.Getenv("TOTE_REDACT") == "1",
		Home:    home,
		EnvFile: filepath.Join(home, ".config", "tote", ".env"),
	}
	c.StateDir = StateDir()
	var errs []error
	if v := os.Getenv("TOTE_DAILY_USD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f > 0) || math.IsInf(f, 0) { // !(f > 0) also rejects NaN
			errs = append(errs, fmt.Errorf("TOTE_DAILY_USD=%q is not a positive dollar amount (use TOTE_CHECK=0 to turn tote off)", v))
		} else {
			c.DailyUSD, c.DailySet = f, true
		}
	}
	if v := os.Getenv("TOTE_SAMPLE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= 0 && f <= 1) {
			errs = append(errs, fmt.Errorf("TOTE_SAMPLE=%q is not a fraction from 0 to 1", v))
		} else {
			c.Sample = f
		}
	}
	if v := os.Getenv("TOTE_SKIP_IF"); v != "" {
		c.SkipIfRaw = v
		c.SkipIf, c.SkipIfErr = regexp.Compile(v) // compiled once; an invalid pattern fails closed at hook time
	}
	return c, errors.Join(errs...)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// StateDir is where the log, the send-back markers and the response cache live: $XDG_STATE_HOME/tote, or
// ~/.local/state/tote.
func StateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "tote")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tote")
}

// APIKey returns ANTHROPIC_API_KEY from the environment, or from the key file (a Stop hook does not inherit a
// shell's environment). A key file that group or others can read is refused.
func (c *Config) APIKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); k != "" {
		return k, nil
	}
	f, err := os.Open(c.EnvFile)
	if err != nil {
		return "", &CheckError{Class: "no_api_key", Err: err}
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Mode().Perm()&0o077 != 0 {
		return "", &CheckError{Class: "env_file_permissions",
			Err: fmt.Errorf("%s is readable by group or others (mode %04o); run: chmod 600 %s", c.EnvFile, st.Mode().Perm(), c.EnvFile)}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		if v, ok := strings.CutPrefix(line, "ANTHROPIC_API_KEY="); ok {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			if v != "" {
				return v, nil
			}
		}
	}
	return "", &CheckError{Class: "no_api_key", Err: fmt.Errorf("no ANTHROPIC_API_KEY in %s", c.EnvFile)}
}

// ensureDir creates a directory, and its missing parents, with mode 0700.
func ensureDir(dir string) error { return os.MkdirAll(dir, 0o700) }

// writeFile0600 writes a file atomically (temp file and rename) with mode 0600, creating the directory with 0700.
func writeFile0600(path string, data []byte) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return os.Chmod(path, 0o600)
}
