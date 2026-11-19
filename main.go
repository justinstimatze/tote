// Command tote is a Claude Code Stop hook that catches guesses stated as fact. See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/justinstimatze/tote/internal/tote"
)

// version is set at build time: go build -ldflags "-X main.version=$(git describe --tags --always --dirty)".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return rev[:min(12, len(rev))] + dirty
	}
	return "dev"
}

const usage = `usage:
  tote hook                              the Stop hook (reads the hook JSON on stdin)
  tote install [--remove] PROJECT_DIR    add tote's Stop hook to PROJECT_DIR/.claude/settings.local.json, or remove it
  tote check TRANSCRIPT "REPLY START" [--json]
                                         check one past reply and print its flags
  tote outcomes                          what happened to each flagged sentence
  tote misses [--project NAME] [--until YYYY-MM-DD]
                                         guesses tote let through
  tote version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "hook":
		tote.RunHook(os.Stdin, os.Stdout) // never fails the turn: errors become a notice
	case "install":
		err = runInstall(args)
	case "check":
		err = runCheck(args, os.Stdout)
	case "outcomes":
		err = tote.RunOutcomes(tote.StateDir(), projectsDir(), os.Stdout)
	case "misses":
		err = runMisses(args)
	case "version", "--version", "-v":
		fmt.Println("tote " + buildVersion())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "tote: unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tote:", err)
		os.Exit(1)
	}
}

func projectsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func runInstall(args []string) error {
	remove := false
	var dirs []string
	for _, a := range args {
		if a == "--remove" {
			remove = true
		} else if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown flag %s", a)
		} else {
			dirs = append(dirs, a)
		}
	}
	if len(dirs) != 1 {
		return errors.New("install takes one PROJECT_DIR")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return tote.Install(dirs[0], exe, remove, tote.InstallOptions{SkipIf: os.Getenv("TOTE_SKIP_IF"), Redact: os.Getenv("TOTE_REDACT") == "1"}, os.Stdout)
}

func runMisses(args []string) error {
	var o tote.MissesOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project", "--until":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", args[i])
			}
			if args[i] == "--project" {
				o.Project = args[i+1]
			} else {
				o.Until = args[i+1]
			}
			i++
		default:
			return fmt.Errorf("unknown argument %s", args[i])
		}
	}
	return tote.RunMisses(tote.StateDir(), projectsDir(), o, os.Stdout)
}

// checkJSON is what `tote check --json` prints, for a private eval script to read.
type checkJSON struct {
	Model     string         `json:"model"`
	Rows      []string       `json:"rows"`
	Flags     []tote.Flag    `json:"flags"`
	Judged    []tote.Judged  `json:"judged"`
	Verdicts  []tote.Verdict `json:"verdicts"`
	Cost      float64        `json:"cost"`
	CostKnown bool           `json:"cost_known"`
	Usage     tote.Usage     `json:"usage"`
	Phrases   map[int]string `json:"phrases"`
	Segment   int            `json:"segment"`
}

func runCheck(args []string, out io.Writer) error {
	asJSON := false
	var pos []string
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return errors.New(`check takes TRANSCRIPT and "REPLY START"`)
	}
	cfg, err := tote.LoadConfig()
	if err != nil {
		return err
	}
	var rend *tote.Rendered
	seg := 0
	for events := range tote.Segments(pos[0]) {
		if r, err := tote.Render(events, tote.Target{Prefix: pos[1]}, tote.Window); err == nil {
			rend = r
			break
		}
		seg++
	}
	if rend == nil {
		return errors.New("reply not found")
	}
	if cfg.SkipIf != nil && cfg.SkipIf.MatchString(strings.ReplaceAll(rend.Stable, tote.Chunk, "\n\n")+"\n\n"+rend.Tail) {
		return errors.New("the transcript window matches TOTE_SKIP_IF, so it is not sent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := (&tote.Checker{Cfg: cfg}).Check(ctx, rend)
	if err != nil {
		return err
	}
	if asJSON {
		j := checkJSON{res.Model, res.Rows, res.Flags, res.Judged, res.Verdicts, res.Cost, res.CostKnown, res.Usage, map[int]string{}, seg}
		for _, f := range res.Flags {
			j.Phrases[f.Row] = f.Phrase()
		}
		if j.Flags == nil {
			j.Flags = []tote.Flag{}
		}
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		return enc.Encode(j)
	}
	for _, f := range res.Flags {
		row := []rune(res.Rows[f.Row-1])
		if len(row) > 120 {
			row = row[:120]
		}
		fmt.Fprintf(out, "%d. %s\n   %s: %s\n", f.Row, string(row), f.Phrase(), f.Why)
	}
	fmt.Fprintf(out, "%d flagged of %d sentences, $%.3f\n", len(res.Flags), len(res.Rows), res.Cost)
	return nil
}
