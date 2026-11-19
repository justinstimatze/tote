package tote

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Outcomes reads what happened to each flagged sentence after tote sent a reply back: from the transcripts, with no
// model call.
//
// For every block (a "Stop hook feedback: tote..." message in a session transcript) it takes the flagged sentences
// named in it, the agent's turn that answered it, and what that turn cost. Each flag gets one outcome:
//
//	corrected   the answer has a "Correction (n):" line for it (or, for unnumbered corrections, one that matches it)
//	checked     a tool call followed and no correction names it: re-checked and left standing
//	ignored     no tool call and no correction
//
// Older blocks, from before corrections-only answers, are scored by whether the sentence still appears as written:
// changed_after_check, confirmed_after_check, dropped_or_hedged, ignored.
//
// Each correction also gets a type, by code on the flag and the correction: existence (the correction negates what the
// sentence asserted), value (a number, path or name in the correction is not in the sentence), scope (the sentence had
// every/all/only/never/always/can and the correction drops it), other.
var (
	blockRe      = regexp.MustCompile(`^Stop hook feedback:\s*\ntote(?: ledger)?: (?:\d+ (?:claim|sentence)|re-check these sentences)`)
	flagRe       = regexp.MustCompile(`(?m)^(?:- |\d+\. )(?:row \d+, )?"(.+?)"[:\s(]`) // the quoted sentence or span of each flag line
	correctionRe = regexp.MustCompile(`(?m)^Correction(?: \((\d+)\))?:\s*(.+)$`)
	rebutRe      = regexp.MustCompile(`(?i)false flag|wrong flag|flags? (?:is|are|was|were) wrong|flag (?:is|was) false|backing is there|` +
		`(?:is|was) backed|not a guess`)
	negRe = regexp.MustCompile(`(?i)\b(?:doesn't|does not|isn't|is not|wasn't|was not|aren't|don't|do not|didn't|did not|can't|cannot|` +
		`no longer|never|not yet|nothing|none|no \w+)\b`)
	scopeRe = regexp.MustCompile(`(?i)\b(?:every|all|only|never|always|each|any|can)\b`)
)

// TurnCost returns the dollars for the agent's API calls in a turn, one usage per message id (the transcript repeats
// a message per content block). Cache writes bill at 2x for the 1-hour TTL and 1.25x for 5 minutes, reads at 0.1x.
func TurnCost(usages map[string]modelUsage) float64 {
	total := 0.0
	for _, mu := range usages {
		p := Prices[mu.model]
		num := func(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }
		cc, _ := mu.usage["cache_creation"].(map[string]any)
		w1, w5 := num(cc, "ephemeral_1h_input_tokens"), num(cc, "ephemeral_5m_input_tokens")
		w5 += max(0, num(mu.usage, "cache_creation_input_tokens")-w1-w5)
		total += (num(mu.usage, "input_tokens")*p[0] + w1*p[0]*2 + w5*p[0]*1.25 +
			num(mu.usage, "cache_read_input_tokens")*p[0]*0.1 + num(mu.usage, "output_tokens")*p[1]) / 1e6
	}
	return total
}

type modelUsage struct {
	model string
	usage map[string]any
}

// Block is one send-back in a transcript, with the turn that answered it.
type SendBack struct {
	Text  string
	Calls int
	Reply string
	Cost  float64
	// Async: the flags rode along with the user's next message (an async Stop hook), so the turn that answers them
	// would have run anyway and its cost is not a corrections turn
	Async bool
}

type blockState struct {
	text  string
	calls int
	reply string
	use   map[string]modelUsage
	async bool
}

func (b *blockState) done() SendBack {
	return SendBack{b.text, b.calls, b.reply, TurnCost(b.use), b.async}
}

// Blocks streams the send-backs of one transcript. A turn often answers in text first and checks after ("I'm
// re-running both"), so the whole turn is read.
func Blocks(path string, yield func(SendBack)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var cur *blockState
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && (cur != nil || bytes.Contains(line, []byte("Stop hook feedback")) || bytes.Contains(line, []byte("hookAdditionalContext")) ||
			bytes.Contains(line, []byte("async_hook_response"))) {
			handleBlockLine(line, &cur, yield)
		}
		if rerr != nil {
			break
		}
	}
	if cur != nil {
		yield(cur.done())
	}
	return nil
}

// asyncToteContext returns tote's additionalContext from an async Stop hook's attachment entry, or "".
func asyncToteContext(line []byte) string {
	var a struct {
		Attachment struct {
			Type, HookEvent string
			Response        struct {
				HookSpecificOutput struct {
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			} `json:"response"`
		} `json:"attachment"`
	}
	if json.Unmarshal(line, &a) != nil || a.Attachment.Type != "async_hook_response" || a.Attachment.HookEvent != "Stop" {
		return ""
	}
	if c := a.Attachment.Response.HookSpecificOutput.AdditionalContext; strings.HasPrefix(c, "tote") {
		return c
	}
	return ""
}

func handleBlockLine(line []byte, cur **blockState, yield func(SendBack)) {
	e := parseEvent(line)
	if e == nil {
		return
	}
	if e.Subtype == "stop_hook_summary" { // silent send-back: the hook's additionalContext
		var ctx []string
		for _, x := range e.HookAdditionalContext {
			if strings.HasPrefix(x, "tote") {
				ctx = append(ctx, x)
			}
		}
		if len(ctx) > 0 {
			if *cur != nil {
				yield((*cur).done())
			}
			*cur = &blockState{text: "Stop hook feedback:\n" + ctx[0], use: map[string]modelUsage{}}
		}
		return
	}
	if e.Type == "attachment" { // async send-back: logged when it lands, before the message it rides along with
		if ctx := asyncToteContext(line); ctx != "" {
			if *cur != nil {
				yield((*cur).done())
			}
			*cur = &blockState{text: "Stop hook feedback:\n" + ctx, use: map[string]modelUsage{}, async: true}
		}
		return
	}
	if e.Msg == nil {
		return
	}
	if e.Type == "user" {
		if c, ok := e.Msg.ContentString(); ok {
			if *cur != nil && (*cur).async && (*cur).reply == "" && !blockRe.MatchString(c) {
				return // the prompt the flags rode in with: its answer is where the correction lands
			}
			if *cur != nil && e.IsMeta { // Claude Code's nudge after an empty turn belongs to the same turn
				return
			}
			if *cur != nil {
				yield((*cur).done())
			}
			*cur = nil // any other user message ends the turn
			if blockRe.MatchString(c) {
				*cur = &blockState{text: c, use: map[string]modelUsage{}}
			}
			return
		}
	}
	if *cur == nil || e.Type != "assistant" {
		return
	}
	if e.Msg.ID != "" && e.Msg.Usage != nil {
		(*cur).use[e.Msg.ID] = modelUsage{e.Msg.Model, e.Msg.Usage}
	}
	for _, b := range e.Msg.Blocks() {
		switch {
		case b.Type == "tool_use":
			(*cur).calls++
		case b.Type == "text" && strings.TrimSpace(b.Text) != "":
			// every text block of the turn: an async answer puts its correction first and keeps working after it
			if (*cur).reply != "" {
				(*cur).reply += "\n"
			}
			(*cur).reply += b.Text
		}
	}
}

// CorrectionType classifies a correction of a flagged sentence.
func CorrectionType(flag, correction string) string {
	if negRe.MatchString(correction) && !negRe.MatchString(flag) {
		return "existence"
	}
	fn, fi := Nums(flag), identNames(flag)
	cn, ci := Nums(correction), identNames(correction)
	// the correction body has its "(n)" stripped, so even a single digit is a value
	if !subset(cn, fn) || !subset(ci, fi) {
		return "value"
	}
	if words := lowerWords(scopeRe.FindAllString(flag, -1)); len(words) > 0 && !subset(words, lowerWords(scopeRe.FindAllString(correction, -1))) {
		return "scope"
	}
	return "other"
}

func identNames(s string) map[string]bool { n, _ := Identifiers(s); return n }

func lowerWords(ws []string) map[string]bool {
	m := map[string]bool{}
	for _, w := range ws {
		m[strings.ToLower(w)] = true
	}
	return m
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

var wordRe = regexp.MustCompile(`\w{4,}`)

func overlap(a, b string) int {
	sa := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(a), -1) {
		sa[w] = true
	}
	n := 0
	seen := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(b), -1) {
		if sa[w] && !seen[w] {
			seen[w] = true
			n++
		}
	}
	return n
}

// Scored is one flag in a block with its outcome.
type Scored struct {
	Flag, Outcome, Correction, Type string
}

// Score returns one row per flag in a block: its outcome, and the correction and its type when there is one.
func Score(text string, calls int, reply string) []Scored {
	var flags []string
	for _, m := range flagRe.FindAllStringSubmatch(text, -1) {
		flags = append(flags, m[1])
	}
	if len(flags) == 0 {
		return nil
	}
	found := correctionRe.FindAllStringSubmatch(reply, -1)
	if len(found) == 0 && !strings.HasPrefix(strings.TrimSpace(reply), "Checked") && !strings.Contains(text, "re-check these sentences") {
		// an older block, answered by rewriting the reply: score by whether the sentence survived
		r := norm(reply)
		out := make([]Scored, len(flags))
		for i, f := range flags {
			survived := strings.Contains(r, norm(f))
			o := "dropped_or_hedged"
			switch {
			case calls > 0 && survived:
				o = "confirmed_after_check"
			case calls > 0:
				o = "changed_after_check"
			case survived:
				o = "ignored"
			}
			out[i] = Scored{Flag: f, Outcome: o}
		}
		return out
	}
	fixes := map[int]string{}
	for _, m := range found {
		i := -1
		if m[1] != "" {
			n, _ := strconv.Atoi(m[1])
			i = n - 1
		} else {
			best := 0 // no shared words: the correction is not attributed to any flag
			for k := range flags {
				if o := overlap(flags[k], m[2]); o > best {
					best, i = o, k
				}
			}
		}
		if i >= 0 && i < len(flags) {
			if _, ok := fixes[i]; !ok {
				fixes[i] = strings.TrimSpace(m[2])
			}
		}
	}
	out := make([]Scored, len(flags))
	for i, f := range flags {
		s := Scored{Flag: f}
		if fix, ok := fixes[i]; ok {
			s.Outcome, s.Correction, s.Type = "corrected", fix, CorrectionType(f, fix)
		} else if calls > 0 {
			s.Outcome = "checked"
		} else {
			s.Outcome = "ignored"
		}
		out[i] = s
	}
	return out
}

// counter counts strings and lists them by count, ties in first-seen order.
type counter struct {
	order []string
	n     map[string]int
}

func (c *counter) add(k string) {
	if c.n == nil {
		c.n = map[string]int{}
	}
	if _, ok := c.n[k]; !ok {
		c.order = append(c.order, k)
	}
	c.n[k]++
}

func (c *counter) mostCommon() []string {
	out := append([]string(nil), c.order...)
	sort.SliceStable(out, func(i, j int) bool { return c.n[out[i]] > c.n[out[j]] })
	return out
}

// RunOutcomes prints the tally and writes outcomes.csv into the state dir. projectsDir is ~/.claude/projects.
func RunOutcomes(stateDir, projectsDir string, w io.Writer) error {
	sessions := map[string]bool{}
	for _, e := range ReadLog(stateDir) {
		if _, ok := e["flags"]; ok {
			if s, _ := e["session"].(string); s != "" {
				sessions[s] = true
			}
		}
	}
	var names []string
	for s := range sessions {
		names = append(names, s)
	}
	sort.Strings(names)
	type row struct {
		session, flag, outcome, typ, correction string
		calls                                   int
		cost                                    float64
	}
	var rows []row
	turns := counter{}
	rebutted := 0
	var costs []float64
	for _, s := range names {
		paths, _ := filepath.Glob(filepath.Join(projectsDir, "*", s+".jsonl"))
		for _, p := range paths {
			_ = Blocks(p, func(b SendBack) {
				if b.Calls > 0 {
					turns.add("checked")
				} else {
					turns.add("no_tool_call")
				}
				if rebutRe.MatchString(b.Reply) {
					rebutted++
				}
				if strings.Contains(b.Text, "re-check these sentences") && !b.Async {
					costs = append(costs, b.Cost)
				}
				for _, sc := range Score(b.Text, b.Calls, b.Reply) {
					rows = append(rows, row{s, sc.Flag, sc.Outcome, sc.Type, sc.Correction, b.Calls, round(b.Cost, 4)})
				}
			})
		}
	}
	if err := ensureDir(stateDir); err != nil {
		return err
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"session", "flag", "calls", "outcome", "type", "correction", "turn_cost"})
	for _, r := range rows {
		_ = cw.Write([]string{r.session, r.flag, strconv.Itoa(r.calls), r.outcome, r.typ, r.correction, strconv.FormatFloat(r.cost, 'f', -1, 64)})
	}
	cw.Flush()
	out := filepath.Join(stateDir, "outcomes.csv")
	if err := writeFile0600(out, buf.Bytes()); err != nil {
		return err
	}
	var n, types counter
	rs := map[string]bool{}
	for _, r := range rows {
		n.add(r.outcome)
		if r.typ != "" {
			types.add(r.typ)
		}
		rs[r.session] = true
	}
	sum := 0.0
	for _, c := range costs {
		sum += c
	}
	fmt.Fprintf(w, "%d blocked turns: %d with a tool call, %d without; %d say a flag was wrong\n",
		turns.n["checked"]+turns.n["no_tool_call"], turns.n["checked"], turns.n["no_tool_call"], rebutted)
	fmt.Fprintf(w, "%d corrections-only turns cost $%.2f to answer ($%.3f a turn)\n", len(costs), sum, sum/float64(max(1, len(costs))))
	for _, k := range n.mostCommon() {
		fmt.Fprintf(w, "  %-22s %4d  (%.0f%%)\n", k, n.n[k], 100*float64(n.n[k])/float64(max(1, len(rows))))
	}
	if len(types.order) > 0 {
		var parts []string
		for _, k := range types.mostCommon() {
			parts = append(parts, fmt.Sprintf("%s %d", k, types.n[k]))
		}
		fmt.Fprintf(w, "  correction types: %s\n", strings.Join(parts, ", "))
	}
	fmt.Fprintf(w, "%d flags from %d sessions -> %s\n", len(rows), len(rs), out)
	return nil
}
