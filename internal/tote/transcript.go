package tote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Window is the number of transcript characters shown to the checker, besides instructions and memory (~30k tokens).
const Window = 120_000

// ToolCap is the number of characters of each tool output shown to the checker; quotes are checked against the full output.
const ToolCap = 60000

// Chunk separates cache chunks inside the stable part; the API call sends each as its own content block.
const Chunk = "\x1e"

// Event is one transcript line. Fields that are not the expected shape are left empty rather than failing the parse.
type Event struct {
	Type, Subtype, UUID, Timestamp string
	IsMeta                         bool
	Msg                            *Message
	Attachment                     map[string]any
	HookAdditionalContext          []string
}

// Message is the "message" object of a user or assistant event.
type Message struct {
	ID, Model string
	Content   json.RawMessage
	Usage     map[string]any
}

// Block is one content block of a message.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

func parseEvent(line []byte) *Event {
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	e := &Event{}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	e.Type, e.Subtype, e.UUID, e.Timestamp = str("type"), str("subtype"), str("uuid"), str("timestamp")
	_ = json.Unmarshal(m["isMeta"], &e.IsMeta)
	var mm map[string]json.RawMessage
	if json.Unmarshal(m["message"], &mm) == nil && mm != nil {
		msg := &Message{Content: mm["content"]}
		_ = json.Unmarshal(mm["id"], &msg.ID)
		_ = json.Unmarshal(mm["model"], &msg.Model)
		_ = json.Unmarshal(mm["usage"], &msg.Usage)
		e.Msg = msg
	}
	var a map[string]any
	if json.Unmarshal(m["attachment"], &a) == nil {
		e.Attachment = a
	}
	var ctx []any
	if json.Unmarshal(m["hookAdditionalContext"], &ctx) == nil {
		for _, x := range ctx {
			if s, ok := x.(string); ok {
				e.HookAdditionalContext = append(e.HookAdditionalContext, s)
			}
		}
	}
	return e
}

// ContentString returns the content when it is a plain string.
func (m *Message) ContentString() (string, bool) {
	var s string
	if len(m.Content) > 0 && m.Content[0] == '"' && json.Unmarshal(m.Content, &s) == nil {
		return s, true
	}
	return "", false
}

// Blocks returns the content blocks when the content is a list.
func (m *Message) Blocks() []Block {
	var raws []json.RawMessage
	if len(m.Content) == 0 || m.Content[0] != '[' || json.Unmarshal(m.Content, &raws) != nil {
		return nil
	}
	var out []Block
	for _, r := range raws {
		var b Block
		if json.Unmarshal(r, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

// textOf flattens a tool_result content (a string or a list of blocks) into text.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []any
	if json.Unmarshal(raw, &list) != nil {
		return ""
	}
	parts := make([]string, 0, len(list))
	for _, x := range list {
		switch v := x.(type) {
		case map[string]any:
			t, _ := v["text"].(string)
			parts = append(parts, t)
		case string:
			parts = append(parts, v)
		default:
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return strings.Join(parts, "\n")
}

type offLine struct {
	off  int64
	line []byte
}

// backward yields (offset, line) for the lines of a file from `end` (negative: the end of file) back to the start,
// without reading it whole. The last item is the first line.
func backward(path string, end int64) iter.Seq2[int64, []byte] {
	const chunk = 1 << 20
	return func(yield func(int64, []byte) bool) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		pos := end
		if end < 0 {
			if pos, err = f.Seek(0, io.SeekEnd); err != nil {
				return
			}
		}
		var tail []byte
		for pos > 0 {
			step := min(int64(chunk), pos)
			pos -= step
			buf := make([]byte, step)
			if _, err := f.ReadAt(buf, pos); err != nil && err != io.EOF {
				return
			}
			parts := bytes.Split(append(buf, tail...), []byte("\n"))
			tail = parts[0] // may be cut mid-line; completed by the next chunk
			at := pos + int64(len(parts[0])) + 1
			offs := make([]offLine, 0, len(parts)-1)
			for _, x := range parts[1:] {
				offs = append(offs, offLine{at, x})
				at += int64(len(x)) + 1
			}
			for i := len(offs) - 1; i >= 0; i-- {
				if !yield(offs[i].off, offs[i].line) {
					return
				}
			}
		}
		yield(0, tail)
	}
}

// Segments yields the compaction segments of a transcript, newest first, each as a list of events.
//
// It reads backward from the end, so the newest segment, the one the hook needs, costs only its own size. Never
// read a whole transcript into memory.
func Segments(path string) iter.Seq[[]*Event] {
	instructionsBefore := func(a int64) *Event {
		// CLAUDE.md and memory stay in the system prompt after compaction but are logged once (per session start
		// or resume), so the newest copy logged before the segment is carried into it
		for _, line := range backward(path, a) {
			if bytes.Contains(line, []byte(`"type":"instructions"`)) {
				if e := parseEvent(line); e != nil {
					return e
				}
			}
		}
		return nil
	}
	instructionsAfter := func(b int64) *Event {
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		if _, err := f.Seek(b, io.SeekStart); err != nil {
			return nil
		}
		data, _ := io.ReadAll(f)
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.Contains(line, []byte(`"type":"instructions"`)) {
				if e := parseEvent(line); e != nil {
					return e
				}
			}
		}
		return nil
	}
	return func(yield func([]*Event) bool) {
		segEnd := int64(-1)
		var lines [][]byte // newest first
		for off, line := range backward(path, -1) {
			if segEnd < 0 {
				segEnd = off + int64(len(line))
				if len(line) > 0 {
					segEnd++
				}
			}
			if bytes.Contains(line, []byte(`"compact_boundary"`)) {
				// the boundary line closes the older segment; this one starts on the next line. Messages compaction kept
				// verbatim are logged above the boundary, listed only by uuid in it: the agent still sees them, so they
				// open this segment
				lines = append(lines, preserved(path, off, line)...)
				if !yield(buildSegment(off+int64(len(line))+1, segEnd, lines, instructionsBefore, instructionsAfter)) {
					return
				}
				segEnd, lines = off+int64(len(line))+1, [][]byte{line}
			} else {
				lines = append(lines, line)
			}
		}
		if segEnd >= 0 {
			yield(buildSegment(0, segEnd, lines, instructionsBefore, instructionsAfter))
		}
	}
}

// preserved returns lines above a compact boundary that compaction kept verbatim, newest first.
func preserved(path string, off int64, boundary []byte) [][]byte {
	var b map[string]any
	if json.Unmarshal(boundary, &b) != nil {
		return nil
	}
	meta, _ := b["compactMetadata"].(map[string]any)
	kept, _ := meta["preservedMessages"].(map[string]any)
	want := map[string]bool{}
	for _, key := range []string{"allUuids", "uuids"} {
		if l, ok := kept[key].([]any); ok && len(l) > 0 {
			for _, u := range l {
				if s, ok := u.(string); ok {
					want[s] = true
				}
			}
			break
		}
	}
	seg, _ := meta["preservedSegment"].(map[string]any)
	head, _ := seg["headUuid"].(string)
	var out [][]byte
	for _, line := range backward(path, off) {
		if len(want) == 0 {
			break
		}
		if !bytes.Contains(line, []byte(`"uuid"`)) {
			continue
		}
		var x struct {
			UUID string `json:"uuid"`
		}
		if json.Unmarshal(line, &x) != nil {
			continue
		}
		if want[x.UUID] {
			out = append(out, line)
			delete(want, x.UUID)
			if x.UUID == head {
				break
			}
		}
	}
	return out
}

func buildSegment(a, b int64, lines [][]byte, before func(int64) *Event, after func(int64) *Event) []*Event {
	first := before(a)
	if first == nil {
		if later := after(b); later != nil {
			// only a later copy exists (e.g. logged at a resume): use it, labelled as such
			if files, ok := later.Attachment["files"].([]any); ok {
				for _, f := range files {
					if fm, ok := f.(map[string]any); ok {
						p, _ := fm["path"].(string)
						fm["path"] = p + " (copy logged later in the session; may differ from what this segment saw)"
					}
				}
			}
			first = later
		}
	}
	var events []*Event
	if first != nil {
		events = append(events, first)
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}
		if e := parseEvent(lines[i]); e != nil {
			events = append(events, e)
		}
	}
	return events
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// contextOf returns text the agent was given outside the chat: instruction files (CLAUDE.md, MEMORY.md), hook
// injections, hook system messages and attached files. Transcripts record these as `attachment` events.
func contextOf(e *Event) [][2]string {
	if e.Type != "attachment" || e.Attachment == nil {
		return nil
	}
	a := e.Attachment
	hookName := func() string {
		if n := str(a, "hookName"); n != "" {
			return n
		}
		return "hook"
	}
	switch str(a, "type") {
	case "instructions":
		var out [][2]string
		files, _ := a["files"].([]any)
		for _, f := range files {
			if fm, ok := f.(map[string]any); ok {
				p := str(fm, "path")
				if p == "" {
					p = "instructions"
				}
				out = append(out, [2]string{p, str(fm, "content")})
			}
		}
		return out
	case "hook_success":
		ev := str(a, "hookEvent")
		if ev != "UserPromptSubmit" && ev != "SessionStart" {
			return nil
		}
		// stdout of a prompt or session hook, which the agent sees (e.g. the context size line)
		if c, ok := a["content"].(string); ok && strings.TrimSpace(c) != "" {
			return [][2]string{{hookName() + " hook", c}}
		}
	case "hook_additional_context", "hook_system_message":
		switch c := a["content"].(type) {
		case []any:
			parts := make([]string, len(c))
			for i, x := range c {
				parts[i] = fmt.Sprint(x)
			}
			return [][2]string{{hookName() + " hook", strings.Join(parts, "\n")}}
		case nil:
			return [][2]string{{hookName() + " hook", ""}}
		default:
			return [][2]string{{hookName() + " hook", fmt.Sprint(c)}}
		}
	case "file":
		var c string
		if v, ok := a["content"].(string); ok {
			c = v
		} else {
			b, _ := json.Marshal(a["content"])
			c = string(b)
		}
		return [][2]string{{"attached file " + str(a, "filename"), c}}
	case "session_context": // what the harness attached at session start, e.g. gitStatus with the current branch
		if m, ok := a["context"].(map[string]any); ok { // the user's email address is identity, not evidence
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var out [][2]string
			for _, k := range keys {
				if k != "userEmail" {
					out = append(out, [2]string{"session context: " + k, fmt.Sprint(m[k])})
				}
			}
			return out
		}
		return [][2]string{{"session context", fmt.Sprint(a["context"])}}
	}
	return nil
}

// Source is something a quote can be checked against: a tool output, a context block, a background-agent result.
type Source struct {
	Text  string // full text
	Pos   int    // position in the rendered block list
	Input string // the tool call's input as JSON, kept to catch self-citation

	normText, normNoLine, normInput, normDecoded string
	prepared                                     bool
}

func (s *Source) prep() {
	if s.prepared {
		return
	}
	s.prepared = true
	s.normText, s.normNoLine = norm(s.Text), norm(lineNo.ReplaceAllString(s.Text, ""))
	s.normInput, s.normDecoded = norm(s.Input), norm(callText(s.Input))
}

// Tools holds sources by id, remembering insertion order.
type Tools struct {
	M     map[string]*Source
	Order []string
}

// NewTools returns an empty set of sources.
func NewTools() *Tools { return &Tools{M: map[string]*Source{}} }

// Set adds or replaces a source; a replaced source keeps its original place in the order.
func (t *Tools) Set(id string, s *Source) {
	if _, ok := t.M[id]; !ok {
		t.Order = append(t.Order, id)
	}
	t.M[id] = s
}

// Get returns the source with that id, or nil.
func (t *Tools) Get(id string) *Source { return t.M[id] }

// User is a user message and its position.
type User struct {
	Text string
	Pos  int
}

// Summary is the session summary left by compaction; Pos is -1 when there is none.
type Summary struct {
	Text string
	Pos  int
}

// Target says which assistant text block is the reply under audit: the block whose text equals Full when Exact,
// otherwise the first that starts with Prefix.
type Target struct {
	Full, Prefix string
	Exact        bool
}

func (t Target) matches(text string) bool {
	if t.Exact {
		return strings.TrimSpace(text) == strings.TrimSpace(t.Full)
	}
	return strings.HasPrefix(text, t.Prefix)
}

// Rendered is a transcript text up to the reply, with every citable source labelled.
type Rendered struct {
	Stable, Tail string // Stable is cache chunks joined by Chunk
	Reply        string
	At           int // position of the reply: evidence must come from before it
	Tools        *Tools
	Users        []User
	Summary      Summary
}

// ErrReplyNotFound means no assistant reply in the segment matched the target.
var ErrReplyNotFound = errors.New("reply not found in this segment")

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// capped shows at most n characters of s, saying how many more there were.
func capped(s string, n int) string {
	if len(s) <= n { // bytes >= characters, so this is safely under the cap
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("\n[... %d more characters]", len(r)-n)
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

var (
	taskResult = regexp.MustCompile(`(?s)<result>(.*?)</result>`)
	taskID     = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
)

// Render builds one transcript text up to the target reply, labelling each source in place, windowed to
// windowChars (0 means no window: everything is in Stable).
func Render(events []*Event, target Target, windowChars int) (*Rendered, error) {
	var last *Rendered
	var out []string
	tools := NewTools()
	var users []User
	summary := Summary{Pos: -1}
	inputs := map[string]string{}
	seen := map[string]bool{}
	pinned := map[int]bool{}
	for _, e := range events {
		for _, lt := range contextOf(e) {
			label, text := lt[0], lt[1]
			if strings.TrimSpace(text) == "" || seen[text] {
				continue
			}
			seen[text] = true
			cid := fmt.Sprintf("ctx-%d", len(seen))
			tools.Set(cid, &Source{Text: text, Pos: len(out)})
			if at := str(e.Attachment, "type"); at == "instructions" || at == "session_context" {
				pinned[len(out)] = true // CLAUDE.md and memory stay in view however long the session
			}
			out = append(out, fmt.Sprintf("[CONTEXT %s] %s\n%s", cid, label, capped(text, ToolCap)))
		}
		if e.Type == "attachment" && e.Attachment != nil && str(e.Attachment, "type") == "queued_command" {
			if origin, _ := e.Attachment["origin"].(map[string]any); str(origin, "kind") == "human" {
				p := str(e.Attachment, "prompt")
				users = append(users, User{p, len(out)}) // a message the user sent while the agent was mid-turn
				out = append(out, "[USER, sent mid-turn]\n"+p)
				continue
			}
		}
		if e.Msg == nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		if c, ok := e.Msg.ContentString(); ok {
			if e.Type != "user" {
				continue
			}
			switch {
			case summary.Pos < 0 && strings.HasPrefix(c, "This session is being continued"):
				summary = Summary{c, len(out)}
				pinned[len(out)] = true
				out = append(out, "[SESSION SUMMARY]\n"+c)
			case strings.HasPrefix(c, "<task-notification>"):
				// a background agent's result: the agent read it, so a reply relaying it is backed by it
				if body := taskResult.FindStringSubmatch(c); body != nil {
					name := fmt.Sprint(len(out))
					if m := taskID.FindStringSubmatch(c); m != nil {
						name = m[1]
					}
					cid := "task-" + name
					full := strings.TrimSpace(body[1])
					tools.Set(cid, &Source{Text: full, Pos: len(out)})
					out = append(out, fmt.Sprintf("[CONTEXT %s] background agent result\n%s", cid, capped(full, ToolCap)))
				}
			case !strings.HasPrefix(c, "<"):
				users = append(users, User{c, len(out)})
				out = append(out, "[USER]\n"+c)
			}
			continue
		}
		for _, b := range e.Msg.Blocks() {
			switch {
			case b.Type == "text" && e.Type == "assistant":
				if target.matches(b.Text) {
					// keep going: the last match is the reply, since an earlier one can share its opening words
					out = append(out, "[REPLY UNDER AUDIT]\n"+b.Text)
					r := &Rendered{Reply: b.Text, At: len(out) - 1, Tools: tools, Users: users, Summary: summary}
					if windowChars > 0 {
						r.Stable, r.Tail = window(out, pinned, windowChars, 20, 40_000)
					} else {
						r.Stable = strings.Join(out, "\n\n")
					}
					last = r
					out[len(out)-1] = "[AGENT]\n" + b.Text
					continue
				}
				out = append(out, "[AGENT]\n"+b.Text)
			case b.Type == "text" && e.Type == "user" && !strings.HasPrefix(b.Text, "<"):
				users = append(users, User{b.Text, len(out)})
				out = append(out, "[USER]\n"+b.Text)
			case b.Type == "tool_use":
				s := compactJSON(b.Input)
				inputs[b.ID] = s
				// head and tail: a long input (an agent brief, a Write) often ends with its output path or constraints
				shown := s
				if len(s) > 8000 {
					if rs := []rune(s); len(rs) > 8000 {
						shown = fmt.Sprintf("%s [... %d chars omitted ...] %s", string(rs[:5000]), len(rs)-7000, string(rs[len(rs)-2000:]))
					}
				}
				out = append(out, fmt.Sprintf("[CALL %s] %s %s", b.ID, b.Name, shown))
			case b.Type == "tool_result":
				full := textOf(b.Content)
				tools.Set(b.ToolUseID, &Source{Text: full, Pos: len(out), Input: inputs[b.ToolUseID]})
				out = append(out, fmt.Sprintf("[OUTPUT %s]\n%s", b.ToolUseID, capped(full, ToolCap)))
			}
		}
	}
	if last != nil {
		return last, nil
	}
	return nil, ErrReplyNotFound
}

// window returns the instruction blocks plus the recent part of the transcript, as (stable, tail).
//
// The stable part is chunks of `step` blocks joined by Chunk. Each chunk goes to the API as its own content block
// with the cache mark on the last, so a later turn re-reads the chunks it shares with an earlier one at a tenth of
// the price and writes only what is new. That only works while the window's start stays put, so the start snaps to a
// grid of block positions every `jump` characters of transcript. The grid never moves as the transcript grows, so the
// start changes once per `jump` characters, and the window shows at most about `jump` characters more than `chars`.
func window(out []string, pinned map[int]bool, chars, step, jump int) (string, string) {
	var body []int
	lens := make([]int, len(out))
	for i := range out {
		lens[i] = runeLen(out[i])
		if !pinned[i] {
			body = append(body, i)
		}
	}
	keep, total := len(body), 0
	for keep > 0 && total+lens[body[keep-1]] <= chars {
		keep--
		total += lens[body[keep]]
	}
	grid, run := []int{0}, 0
	for k, i := range body {
		run += lens[i]
		if run >= jump*len(grid) {
			grid = append(grid, k+1)
		}
	}
	start := 0
	for _, g := range grid {
		if g <= keep && g > start {
			start = g
		}
	}
	cut := start
	if len(body) > 0 {
		cut = max(start, ((len(body)-1)/step)*step)
	}
	var head []string
	for i := range out {
		if pinned[i] {
			head = append(head, out[i])
		}
	}
	if start > 0 {
		head = append(head, fmt.Sprintf("[... %d earlier transcript blocks omitted]", start))
	}
	// chunk edges sit on fixed multiples of `step`, so a chunk, once complete, is the same block on every later turn
	edges := []int{start}
	for e := (start/step + 1) * step; e < cut; e += step {
		edges = append(edges, e)
	}
	edges = append(edges, cut)
	join := func(idx []int) string {
		parts := make([]string, len(idx))
		for k, i := range idx {
			parts[k] = out[i]
		}
		return strings.Join(parts, "\n\n")
	}
	chunks := []string{strings.Join(head, "\n\n")}
	for k := 0; k+1 < len(edges); k++ {
		if a, b := edges[k], edges[k+1]; b > a {
			chunks = append(chunks, join(body[a:b]))
		}
	}
	var kept []string
	for _, c := range chunks {
		if c != "" {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, Chunk), join(body[cut:])
}

var (
	lineStart = regexp.MustCompile(`^\s*([-*|#>]|\d+\.\s|` + "```" + `)`)
	listMark  = regexp.MustCompile(`^([-*]|\d+\.)\s+`)
	lineSplit = regexp.MustCompile(`\r\n|\n|\r`)
)

// unwrap rejoins hard-wrapped prose: a line continues the previous one unless it starts a block or follows a blank.
func unwrap(text string) []string {
	var lines []string
	raw := lineSplit.Split(text, -1)
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1]
	}
	for _, line := range raw {
		if n := len(lines); n > 0 {
			prev := strings.TrimSpace(lines[n-1])
			if prev != "" && strings.TrimSpace(line) != "" && !lineStart.MatchString(line) &&
				!strings.HasPrefix(prev, "|") && !strings.HasPrefix(prev, "#") && !strings.HasPrefix(prev, "```") {
				lines[n-1] = strings.TrimRightFunc(lines[n-1], unicode.IsSpace) + " " + strings.TrimSpace(line)
				continue
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// splitSentences splits at whitespace that follows . ! or ? and precedes a capital, backtick, quote or paren.
func splitSentences(s string) []string {
	rs := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		if !unicode.IsSpace(rs[i]) || unicode.IsSpace(rs[i-1]) || !strings.ContainsRune(".!?", rs[i-1]) {
			continue
		}
		j := i
		for j < len(rs) && unicode.IsSpace(rs[j]) {
			j++
		}
		if j < len(rs) && (rs[j] >= 'A' && rs[j] <= 'Z' || strings.ContainsRune("`\"(", rs[j])) {
			parts = append(parts, string(rs[start:i]))
			start = j
			i = j - 1
		}
	}
	return append(parts, string(rs[start:]))
}

// SplitRows splits a reply into the numbered units the checker answers for: sentences, bullets, table rows, headings.
func SplitRows(reply string) []string {
	var rows []string
	inCode := false
	for _, line := range unwrap(reply) {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "```") {
			inCode = !inCode
			continue
		}
		if s == "" || inCode || strings.Trim(s, "|-: ") == "" {
			continue
		}
		if strings.HasPrefix(s, "|") || strings.HasPrefix(s, "#") {
			rows = append(rows, s)
			continue
		}
		for _, x := range splitSentences(listMark.ReplaceAllString(s, "")) {
			if strings.TrimSpace(x) != "" {
				rows = append(rows, x)
			}
		}
	}
	return rows
}

func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

var lineNo = regexp.MustCompile(`(?m)^\s*\d+(?:\t|→)`)

// FallbackMin is the characters a quote needs before an output other than the cited one may back it.
const FallbackMin = 40

// callText returns a tool call's input as plain text: every string value, decoded, one after another.
func callText(raw string) string {
	if raw == "" {
		return ""
	}
	var d any
	if json.Unmarshal([]byte(raw), &d) != nil {
		return raw
	}
	var vals []string
	var walk func(x any)
	walk = func(x any) {
		switch v := x.(type) {
		case string:
			vals = append(vals, v)
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(v[k])
			}
		case []any:
			for _, y := range v {
				walk(y)
			}
		}
	}
	walk(d)
	return strings.Join(vals, "\n")
}

// Claim is a quote the checker cites as backing for a sentence.
type Claim struct {
	Quote, CallID string
	Backing       string // tool_output, tool_call, context, user_message, summary
	InInput       bool   // set by Verify: the quote was found only in the call's input
	FallbackCall  string // set by Verify: another output rescued a long quote
}

// Verify reports whether the quote is an exact substring of the cited source, and the source comes before the reply
// at position `at`.
func Verify(c *Claim, at int, tools *Tools, users []User, summary Summary) bool {
	q := norm(c.Quote)
	if q == "" {
		return false
	}
	inInput := func(s *Source) bool {
		// the input is stored as JSON, where quotes, newlines and non-ASCII are escaped; match the decoded values too
		s.prep()
		return s.Pos < at && (strings.Contains(s.normInput, q) || strings.Contains(s.normDecoded, q))
	}
	has := func(s *Source) bool {
		// Claude Code's Read output starts each line with "N\t"; a quote that spans lines skips those prefixes
		s.prep()
		return s.Pos < at && (strings.Contains(s.normText, q) || strings.Contains(s.normNoLine, q))
	}
	inUsers := func() bool {
		for _, u := range users {
			if u.Pos < at && strings.Contains(norm(u.Text), q) {
				return true
			}
		}
		return false
	}
	inSummary := summary.Pos >= 0 && summary.Pos < at && strings.Contains(norm(summary.Text), q)
	switch c.Backing {
	case "tool_call":
		// the checker sometimes files a quote under the wrong half of the same call, so the cited call's input and
		// output each back a quote from the other. A quote found only in a call's input is the agent's own words
		if src := tools.Get(c.CallID); src != nil {
			if inInput(src) {
				c.InInput = true
				return true
			}
			return has(src)
		}
		return false
	case "tool_output", "context":
		src := tools.Get(c.CallID)
		if src != nil && has(src) {
			return true
		}
		if src != nil && inInput(src) {
			c.InInput = true
			return true
		}
		// the checker sometimes names the wrong call id for a quote it copied exactly. Only a long quote may be rescued
		// by another output, since a short one ("passed", "main") turns up somewhere in any long session
		if runeLen(q) < FallbackMin {
			return false
		}
		for _, id := range tools.Order {
			if has(tools.M[id]) {
				c.FallbackCall = id
				return true
			}
		}
		return false
	case "user_message":
		// a user message from before the last compaction survives only as a quote in the summary
		return inUsers() || inSummary
	case "summary":
		return inSummary || inUsers()
	}
	return false
}

var numRe = regexp.MustCompile(`(?:^|[^\w.])(\d[\d,]*(?:\.\d+)?%?)`)

// Nums returns the numbers in s, without commas or a percent sign.
func Nums(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range numRe.FindAllStringSubmatch(s, -1) {
		out[strings.ReplaceAll(strings.TrimSuffix(m[1], "%"), ",", "")] = true
	}
	return out
}

var idPatterns = []*regexp.Regexp{
	regexp.MustCompile("`([^`\n]{2,80})`"), // backticked code, paths, commands
	// a path: rooted (~/, ./, /) or with two slashes or a file extension; "pause/play" in prose is not one
	regexp.MustCompile(`(?:^|[^\w/])((?:[~.]?/[\w.-]+(?:/[\w.-]+)*|[\w.-]+/[\w.-]+/[\w./-]+|[\w.-]+/[\w/-]*\.\w{1,5}\b))`),
	regexp.MustCompile(`\b[\w-]+\.(?:py|go|ts|tsx|js|md|json|jsonl|toml|yaml|yml|sh|txt|csv|html|sql|rs)\b`), // a file name
	regexp.MustCompile(`\b[a-z][a-z0-9]*_[a-z0-9_]+\b`),                                                      // snake_case
	regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z0-9]*\b`),                                                        // camelCase
}

// Identifiers returns the checkable names and numbers in a claim's span. Single digits are left out of the numbers:
// they are mostly list numbering.
func Identifiers(span string) (names, numbers map[string]bool) {
	names, numbers = map[string]bool{}, map[string]bool{}
	for _, p := range idPatterns {
		for _, m := range p.FindAllStringSubmatch(span, -1) {
			n := m[0]
			if len(m) > 1 {
				n = m[1]
			}
			if n = strings.TrimSpace(n); runeLen(n) > 2 {
				names[n] = true
			}
		}
	}
	for n := range Nums(span) {
		if len(n) != 1 {
			numbers[n] = true
		}
	}
	return
}
