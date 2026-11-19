package tote

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Misses finds the guesses tote let through: the user asked for a double-check and the agent then retracted something.
//
// For each user prompt that asks for a re-check ("are you sure", "double check", "that's wrong"), it reads the agent's
// answer for a retraction, and joins the reply being questioned to the hook log entry for it:
//
//	passed              checked, nothing flagged: a retraction here is a miss
//	flagged             checked and sent back, and the user still had to ask
//	skipped: <reason>   not checked (short reply, budget spent, TOTE_SKIP_IF, ...)
//	no hook run         no log entry between that reply and the prompt (tote not installed, or the hook did not fire)
var (
	// a prompt that questions the reply just given: "double check" alone is often an instruction for new work, so it
	// counts only when it points back ("double check that", "double check your numbers") or the prompt is a question
	broadRe = regexp.MustCompile(`(?i)\b(?:double[- ]?check|are you sure|you sure\?|verify|that'?s (?:wrong|not right|incorrect)|check again|` +
		`is that (?:right|true|correct)|did you (?:check|verify|actually))`)
	askRe = regexp.MustCompile(`(?i)\b(?:are you sure|you sure\b|that'?s (?:wrong|not right|incorrect|not true)|is that (?:right|true|correct)|` +
		`did you (?:check|verify|actually)|double[- ]?check (?:that|this|it|your|those|these|again)|` +
		`check (?:that|this) again|really\?)`)
	automated = []string{"Overnight", "Here is a note offered by a side agent", "[Request interrupted", "Caveat:", "<"}
	// the agent taking back something it said: the miss itself, whatever the prompt that drew it out. Go's regexp has
	// no lookahead, so "without checking" followed by back/with/out/first/in is excluded in findRetraction
	retractRe = regexp.MustCompile(`(?im)^Correction\b|\bI was wrong\b|\bI misread\b|\bI misspoke\b|\b(?:that|which) was wrong\b|` +
		`\bI stand corrected\b|\bI overstated\b|\bwrong (?:earlier|last turn|before)\b|` +
		`\bI said .{0,80}\b(?:but|that was)\b.{0,40}\b(?:wrong|not)\b`)
	withoutRe     = regexp.MustCompile(`(?i)\bwithout checking\b`)
	withoutNotRe  = regexp.MustCompile(`(?i)^ (?:back|with|out|first|in\b)`)
	whenLayouts   = []string{time.RFC3339Nano, "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05"}
	maxPromptSize = 400 // a long prompt is a brief, not a question about the last reply
)

// findRetraction returns the byte span of the leftmost retraction in text.
func findRetraction(text string) (int, int, bool) {
	start, end, ok := 0, 0, false
	if m := retractRe.FindStringIndex(text); m != nil {
		start, end, ok = m[0], m[1], true
	}
	for _, m := range withoutRe.FindAllStringIndex(text, -1) {
		if withoutNotRe.MatchString(text[m[1]:]) {
			continue
		}
		if !ok || m[0] < start {
			start, end, ok = m[0], m[1], true
		}
		break
	}
	return start, end, ok
}

// IsRetraction reports whether text has the agent taking something back.
func IsRetraction(text string) bool { _, _, ok := findRetraction(text); return ok }

func when(ts string) (time.Time, bool) {
	for _, l := range whenLayouts {
		if t, err := time.Parse(l, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

type stamped struct {
	t time.Time
	e map[string]any
}

func hookLog(dir string) map[string][]stamped {
	out := map[string][]stamped{}
	for _, e := range ReadLog(dir) {
		s, _ := e["session"].(string)
		ts, _ := e["time"].(string)
		if t, ok := when(ts); ok && s != "" {
			out[s] = append(out[s], stamped{t, e})
		}
	}
	return out
}

func status(entries []stamped, after *time.Time, before time.Time) string {
	var hit *stamped
	for i := range entries {
		x := entries[i]
		if (after == nil || !x.t.Before(*after)) && !x.t.After(before) {
			hit = &entries[i]
		}
	}
	if hit == nil {
		return "no hook run"
	}
	if s, _ := hit.e["skip"].(string); s != "" {
		return "skipped: " + s
	}
	if n, _ := hit.e["flagged"].(float64); n > 0 {
		return "flagged"
	}
	if fl, _ := hit.e["flags"].([]any); len(fl) > 0 {
		return "flagged"
	}
	return "passed"
}

type missTurn struct {
	replyT  *time.Time
	reply   string
	prompt  string
	promptT *time.Time
	answer  string
}

// turns reads one transcript: one entry per real user prompt, with the reply it answered and the agent's answer.
func turns(path string, yield func(missTurn) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var lastReply string
	var lastT *time.Time
	var prompt *string
	var promptT *time.Time
	var answer []string
	var replyPrev string
	var lastTPrev *time.Time
	emit := func() bool {
		return yield(missTurn{lastTPrev, replyPrev, *prompt, promptT, strings.Join(answer, "\n")})
	}
	tp := func(ts string) *time.Time {
		if t, ok := when(ts); ok {
			return &t
		}
		return nil
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"user"`)) || bytes.Contains(line, []byte(`"assistant"`)) {
			if e := parseEvent(line); e != nil && e.Msg != nil {
				if c, ok := e.Msg.ContentString(); ok && e.Type == "user" {
					if !strings.HasPrefix(c, "<") && !strings.HasPrefix(c, "Stop hook feedback") && !strings.HasPrefix(c, "This session is being continued") {
						if prompt != nil && !emit() {
							return nil
						}
						replyPrev, lastTPrev = lastReply, lastT
						p := c
						prompt, promptT, answer = &p, tp(e.Timestamp), nil
					}
				} else if e.Type == "assistant" {
					for _, b := range e.Msg.Blocks() {
						if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
							lastReply, lastT = b.Text, tp(e.Timestamp)
							if prompt != nil {
								answer = append(answer, b.Text)
							}
						}
					}
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if prompt != nil {
		emit()
	}
	return nil
}

// MissesOptions selects the sessions to read.
type MissesOptions struct {
	Project string // substring of a projects directory name; empty means the sessions tote has logged
	Until   string // YYYY-MM-DD: only prompts before this date
}

func runesSlice(s string, from, to int) string {
	r := []rune(s)
	from = max(0, from)
	to = min(len(r), to)
	if from >= to {
		return ""
	}
	return string(r[from:to])
}

// RunMisses prints the tally and writes misses.csv into the state dir. projectsDir is ~/.claude/projects.
func RunMisses(stateDir, projectsDir string, opt MissesOptions, w io.Writer) error {
	log := hookLog(stateDir)
	var paths []string
	if opt.Project != "" {
		dirs, _ := filepath.Glob(filepath.Join(projectsDir, "*"))
		for _, d := range dirs {
			if strings.Contains(filepath.Base(d), opt.Project) {
				p, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
				paths = append(paths, p...)
			}
		}
	} else {
		var ss []string
		for s := range log {
			ss = append(ss, s)
		}
		sort.Strings(ss)
		for _, s := range ss {
			p, _ := filepath.Glob(filepath.Join(projectsDir, "*", s+".jsonl"))
			paths = append(paths, p...)
		}
	}
	var until *time.Time
	if opt.Until != "" {
		t, ok := when(opt.Until + "T00:00:00+00:00")
		if !ok {
			return fmt.Errorf("--until %q is not a YYYY-MM-DD date", opt.Until)
		}
		until = &t
	}
	type row struct {
		session, time, hook, prompt, retraction string
		strict, retracted                       bool
		replyChars                              int
	}
	var rows []row
	prompts := 0
	for _, path := range paths {
		session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		_ = turns(path, func(t missTurn) bool {
			if until != nil && t.promptT != nil && !t.promptT.Before(*until) {
				return false
			}
			for _, a := range automated {
				if strings.HasPrefix(t.prompt, a) {
					return true
				}
			}
			prompts++
			start, end, hit := findRetraction(t.answer)
			if !broadRe.MatchString(t.prompt) && !hit {
				return true
			}
			r := row{session: session, prompt: runesSlice(strings.Join(strings.Fields(t.prompt), " "), 0, 120),
				strict:    utf8.RuneCountInString(t.prompt) <= maxPromptSize && askRe.MatchString(t.prompt),
				retracted: hit, replyChars: utf8.RuneCountInString(t.reply), hook: "no hook run"}
			if t.promptT != nil {
				r.time = t.promptT.Format(time.RFC3339)
				r.hook = status(log[session], t.replyT, *t.promptT)
			}
			if hit {
				a := utf8.RuneCountInString(t.answer[:start])
				b := utf8.RuneCountInString(t.answer[:end])
				r.retraction = strings.Join(strings.Fields(runesSlice(t.answer, a-40, b+120)), " ")
			}
			rows = append(rows, r)
			return true
		})
	}
	if err := ensureDir(stateDir); err != nil {
		return err
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"session", "time", "hook", "strict", "retracted", "reply_chars", "prompt", "retraction"})
	b2s := func(b bool) string { return map[bool]string{true: "True", false: "False"}[b] }
	strict, ret := 0, 0
	by := map[string][2]int{}
	var order []string
	for _, r := range rows {
		_ = cw.Write([]string{r.session, r.time, r.hook, b2s(r.strict), b2s(r.retracted), strconv.Itoa(r.replyChars), r.prompt, r.retraction})
		if r.strict {
			strict++
		}
		v, seen := by[r.hook]
		if !seen {
			order = append(order, r.hook)
		}
		v[0]++
		if r.retracted {
			ret++
			v[1]++
		}
		by[r.hook] = v
	}
	cw.Flush()
	out := filepath.Join(stateDir, "misses.csv")
	if err := writeFile0600(out, buf.Bytes()); err != nil {
		return err
	}
	fmt.Fprintf(w, "%d user prompts in %d sessions: %d answers retract something the agent said (%.1f per 100 prompts); %d prompts question the last reply outright\n",
		prompts, len(paths), ret, 100*float64(ret)/float64(max(1, prompts)), strict)
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]][0] > by[order[j]][0] })
	for _, k := range order {
		fmt.Fprintf(w, "  %-40s %4d retractions  (%d rows)\n", k, by[k][1], by[k][0])
	}
	fmt.Fprintf(w, "-> %s\n", out)
	return nil
}
