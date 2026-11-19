package tote

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(path, out string, pos int) *Source {
	in, _ := json.Marshal(map[string]any{"file_path": path})
	return &Source{Text: out, Pos: pos, Input: string(in)}
}

func edit(path string, pos int) *Source {
	in, _ := json.Marshal(map[string]any{"file_path": path, "old_string": "a", "new_string": "b"})
	return &Source{Text: "ok", Pos: pos, Input: string(in)}
}

func toolsOf(kv ...any) *Tools {
	t := NewTools()
	for i := 0; i < len(kv); i += 2 {
		t.Set(kv[i].(string), kv[i+1].(*Source))
	}
	return t
}

func writeLines(t *testing.T, lines ...any) string {
	t.Helper()
	var b bytes.Buffer
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type m = map[string]any

func text(s string) m { return m{"type": "text", "text": s} }

func asst(blocks ...m) m { return m{"type": "assistant", "message": m{"content": blocks}} }

// Verify: a quote must really be in the cited source, before the reply.

func TestVerify(t *testing.T) {
	long := strings.Repeat("x", FallbackMin)
	cases := []struct {
		name  string
		tools *Tools
		claim Claim
		at    int
		want  bool
	}{
		{"exact quote", toolsOf("c1", &Source{Text: "total 42 rows", Pos: 1}), Claim{Quote: "42 rows", CallID: "c1", Backing: "tool_output"}, 5, true},
		{"source after the reply", toolsOf("c1", &Source{Text: "total 42 rows", Pos: 9}), Claim{Quote: "42 rows", CallID: "c1", Backing: "tool_output"}, 5, false},
		{"made-up quote", toolsOf("c1", &Source{Text: "total 42 rows", Pos: 1}), Claim{Quote: "43 rows", CallID: "c1", Backing: "tool_output"}, 5, false},
		{"json-escaped input", toolsOf("c1", &Source{Pos: 1, Input: `{"command":"echo \"hi there\""}`}), Claim{Quote: `echo "hi there"`, CallID: "c1", Backing: "tool_call"}, 5, true},
		{"long quote rescued by another output", toolsOf("c1", &Source{Text: "unrelated", Pos: 1}, "c2", &Source{Text: "before " + long + " after", Pos: 2}),
			Claim{Quote: long, CallID: "c1", Backing: "tool_output"}, 5, true},
		{"short quote not rescued", toolsOf("c1", &Source{Text: "unrelated", Pos: 1}, "c2", &Source{Text: "before " + long + " after", Pos: 2}),
			Claim{Quote: "before", CallID: "c1", Backing: "tool_output"}, 5, false},
		{"read output with line numbers", toolsOf("c1", &Source{Text: "     1\tfoo\n     2\tbar", Pos: 1}), Claim{Quote: "foo bar", CallID: "c1", Backing: "tool_output"}, 5, true},
		{"empty quote", toolsOf("c1", &Source{Text: "x", Pos: 1}), Claim{Quote: "  ", CallID: "c1", Backing: "tool_output"}, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Verify(&c.claim, c.at, c.tools, nil, Summary{Pos: -1}); got != c.want {
				t.Fatalf("Verify = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVerifyUserMessageAndSummary(t *testing.T) {
	users := []User{{"please use port 8080", 2}}
	if !Verify(&Claim{Quote: "port 8080", CallID: "user", Backing: "user_message"}, 5, NewTools(), users, Summary{Pos: -1}) {
		t.Error("user message quote not found")
	}
	if Verify(&Claim{Quote: "port 8080", CallID: "user", Backing: "user_message"}, 1, NewTools(), users, Summary{Pos: -1}) {
		t.Error("user message after the reply backed a quote")
	}
	if !Verify(&Claim{Quote: "deadline is May", CallID: "ctx-1", Backing: "summary"}, 5, NewTools(), nil, Summary{"The deadline is May.", 0}) {
		t.Error("summary quote not found")
	}
}

// EditedAfter: a quote from a read goes stale when the file is edited before the reply.

func TestEditedAfter(t *testing.T) {
	cases := []struct {
		name  string
		tools *Tools
		cid   string
		at    int
		want  string
	}{
		{"read then edit is stale", toolsOf("r", read("/a.py", "x = 1", 1), "e", edit("/a.py", 3)), "r", 5, "/a.py"},
		{"edit cited for itself is not stale", toolsOf("e1", edit("/a.py", 2), "e2", edit("/a.py", 3)), "e1", 5, ""},
		{"edit after the reply does not count", toolsOf("r", read("/a.py", "x = 1", 1), "e", edit("/a.py", 7)), "r", 5, ""},
		{"edit to another file does not count", toolsOf("r", read("/a.py", "x = 1", 1), "e", edit("/b.py", 3)), "r", 5, ""},
		{"unknown call", NewTools(), "zz", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EditedAfter(c.cid, c.at, c.tools); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Render.

func TestRenderKeepsTaskNotificationAsCitableContext(t *testing.T) {
	events := []*Event{
		parseEvent([]byte(`{"type":"user","message":{"content":"<task-notification>\n<task-id>abc</task-id>\n<result>Answering as: x\nThe verdict is yes.</result>\n</task-notification>"}}`)),
		parseEvent([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"My reply: the verdict is yes."}]}}`)),
	}
	r, err := Render(events, Target{Prefix: "My reply"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := r.Tools.Get("task-abc")
	if src == nil || !strings.Contains(src.Text, "The verdict is yes.") {
		t.Fatal("task result is not a source")
	}
	if !Verify(&Claim{Quote: "The verdict is yes.", CallID: "task-abc", Backing: "context"}, r.At, r.Tools, r.Users, r.Summary) {
		t.Fatal("task result does not back its quote")
	}
}

func TestRenderDropsOtherSystemText(t *testing.T) {
	events := []*Event{
		parseEvent([]byte(`{"type":"user","message":{"content":"<system-reminder>secret instructions</system-reminder>"}}`)),
		parseEvent([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"My reply."}]}}`)),
	}
	r, err := Render(events, Target{Prefix: "My reply"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Stable+r.Tail, "secret instructions") {
		t.Fatal("system text was rendered")
	}
}

func TestRenderPrefersExactReplyOverEarlierPrefixMatch(t *testing.T) {
	early := "The cache holds 40k characters. " + strings.Repeat("a", 100)
	final := early + " And one more sentence."
	events := []*Event{
		parseEvent([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":` + jsonStr(early) + `}]}}`)),
		parseEvent([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":` + jsonStr(final) + `}]}}`)),
	}
	prefix := early[:80]
	r, err := Render(events, Target{Full: final, Prefix: prefix, Exact: true}, 0)
	if err != nil || r.Reply != final {
		t.Fatalf("exact match found %q, %v", r.Reply, err)
	}
	r, err = Render(events, Target{Prefix: prefix}, 0)
	if err != nil || r.Reply != early {
		t.Fatalf("prefix match found %q, %v", r.Reply, err)
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestWindowChunksStayPutAsTheTranscriptGrows(t *testing.T) {
	mk := func(n int) []string {
		var out []string
		for i := 0; i < n; i++ {
			out = append(out, strings.Repeat("x", 3000)+string(rune('a'+i%26)))
		}
		return out
	}
	s1, _ := window(mk(100), nil, 120_000, 20, 40_000)
	s2, _ := window(mk(105), nil, 120_000, 20, 40_000)
	c1, c2 := strings.Split(s1, Chunk), strings.Split(s2, Chunk)
	if len(c1) < 2 {
		t.Fatalf("expected several chunks, got %d", len(c1))
	}
	for i := 0; i < len(c1)-1; i++ { // every complete chunk is the same block on the later turn
		if c1[i] != c2[i] {
			t.Fatalf("chunk %d changed as the transcript grew", i)
		}
	}
}

func TestSplitRows(t *testing.T) {
	got := SplitRows("The cache is 40k. It holds `x.go` files.\n\n- First point here\n  continued line\n| a | b |\n|---|---|\n```\ncode. Here.\n```\nDone.")
	want := []string{"The cache is 40k.", "It holds `x.go` files.", "First point here continued line", "| a | b |", "Done."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}

// outcomes.

const newBlock = "Stop hook feedback:\ntote: re-check these sentences from your last reply before the user relies on them:\n" +
	"1. \"Every status file is fresh.\" (not sure)\n2. \"It costs $5.\" (not sure)\n"
const oldBlock = "Stop hook feedback:\ntote: 2 sentences need a source:\n- row 3, \"Alpha is 3\": unbacked\n- \"Beta is 4\" (no quote)\n"

func TestBlockAndFlagPatternsMatchOldAndNewWording(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{{newBlock, []string{"Every status file is fresh.", "It costs $5."}}, {oldBlock, []string{"Alpha is 3", "Beta is 4"}}} {
		if !blockRe.MatchString(c.text) {
			t.Errorf("block not matched: %q", c.text)
		}
		var got []string
		for _, m := range flagRe.FindAllStringSubmatch(c.text, -1) {
			got = append(got, m[1])
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("flags %q, want %q", got, c.want)
		}
	}
}

func TestNumberedCorrectionMapsToItsFlag(t *testing.T) {
	rows := Score(newBlock, 2, "Correction (2): It costs $7.")
	if rows[0].Outcome != "checked" || rows[1].Outcome != "corrected" || rows[1].Type != "value" {
		t.Fatalf("%+v", rows)
	}
}

func TestCheckedWithNoToolCallIsIgnored(t *testing.T) {
	rows := Score(newBlock, 0, "Checked.")
	if rows[0].Outcome != "ignored" || rows[1].Outcome != "ignored" {
		t.Fatalf("%+v", rows)
	}
}

func TestCorrectionTypes(t *testing.T) {
	for _, c := range []struct{ flag, fix, want string }{
		{"tote counts Correction turns", "tote doesn't count them yet", "existence"},
		{"6 of 8 were sent back", "5 of 8 were sent back", "value"},
		{"every status file is fresh", "the two status files I checked are fresh", "scope"},
	} {
		if got := CorrectionType(c.flag, c.fix); got != c.want {
			t.Errorf("%q -> %q: got %s, want %s", c.flag, c.fix, got, c.want)
		}
	}
}

func TestTurnCostCountsEachMessageOnceAndPrices1hWrites(t *testing.T) {
	u := m{"input_tokens": 0.0, "output_tokens": 0.0, "cache_read_input_tokens": 0.0, "cache_creation_input_tokens": 1e6,
		"cache_creation": m{"ephemeral_1h_input_tokens": 1e6, "ephemeral_5m_input_tokens": 0.0}}
	if got := TurnCost(map[string]modelUsage{"m1": {"claude-opus-5-5", u}}); got != 8.0 { // 4.00/M input, 2x for a 1-hour write
		t.Fatalf("got %v", got)
	}
}

func TestCostAccounting(t *testing.T) {
	// sonnet: $2/M in, $10/M out; 1M tokens in of which 400k written (2x) and 100k read (0.1x), 100k out
	got, ok := Cost("claude-sonnet-5-5", Usage{In: 1_000_000, CacheWrite: 400_000, CacheRead: 100_000, Out: 100_000})
	want := 0.6*2 + 0.4*2*2 + 0.1*2*0.1 + 0.1*10
	if !ok || got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, ok := Cost("claude-future-9", Usage{In: 1}); ok {
		t.Fatal("an unknown model has a price")
	}
}

// misses.

func TestRetractionPattern(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"I was wrong earlier: the cap exists.", true},
		{"I asserted the default without checking it.", true},
		{"Once green I'll merge without checking back with you.", false},
		{"The gate is wrong, not the walkers.", false},
		{"Correction (1): it is 7.", true},
	} {
		if got := IsRetraction(c.s); got != c.want {
			t.Errorf("%q: got %v", c.s, got)
		}
	}
}

func TestAbsencePattern(t *testing.T) {
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"Part 3 never shows that door.", true},
		{"`script/scenes.json` doesn't exist.", true},
		{"There are no other callers.", true},
		{"Nothing else reads this file.", true},
		{"No one calls this function.", true},
		{"Nothing writes to it.", true},
		{"The only caller is main.", true},
		{"Only main calls this function.", true},
		{"Only the hook uses it.", true},
		{"The key is not in the config.", true},
		{"The door appears in part 3.", false},
		{"I only changed one line.", false},
		{"It runs only when the flag is set.", false},
		{"Someone noted this earlier.", false},
		{"The result is not interesting.", false},
		{"Nothingness is a concept.", false},
	} {
		if got := IsAbsence(c.s); got != c.want {
			t.Errorf("%q: got %v, want %v", c.s, got, c.want)
		}
	}
}

func TestSegmentKeepsMessagesCompactionPreserved(t *testing.T) {
	p := writeLines(t,
		m{"type": "user", "uuid": "u-old", "message": m{"content": "dropped by compaction"}},
		m{"type": "user", "uuid": "u-kept", "message": m{"content": []m{{"type": "tool_result", "tool_use_id": "t1", "content": "alpha 28"}}}},
		m{"type": "system", "subtype": "compact_boundary", "uuid": "b",
			"compactMetadata": m{"preservedSegment": m{"headUuid": "u-kept"}, "preservedMessages": m{"uuids": []string{"u-kept"}}}},
		m{"type": "assistant", "uuid": "a1", "message": m{"content": []m{text("Alpha has 28 sessions.")}}})
	for events := range Segments(p) {
		r, err := Render(events, Target{Prefix: "Alpha has"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Stable, "alpha 28") || strings.Contains(r.Stable, "dropped by compaction") {
			t.Fatalf("rendered %q", r.Stable)
		}
		return
	}
	t.Fatal("no segments")
}

func TestOutcomesReadsSilentSendBack(t *testing.T) {
	ctx := "tote: re-check these sentences from your last reply before the user relies on them:\n1. \"It costs $5.\" (not sure)\n"
	p := writeLines(t,
		m{"type": "system", "subtype": "stop_hook_summary", "hookAdditionalContext": []string{ctx}},
		m{"type": "assistant", "message": m{"id": "m", "content": []m{text("Correction (1): It costs $7.")}}})
	var got []SendBack
	if err := Blocks(p, func(b SendBack) { got = append(got, b) }); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if s := Score(got[0].Text, got[0].Calls, got[0].Reply); len(s) != 1 || s[0].Outcome != "corrected" {
		t.Fatalf("%+v", s)
	}
}

func TestSegmentsAndRenderReadATranscriptFile(t *testing.T) {
	p := writeLines(t, asst(text("My reply here.")))
	n := 0
	for events := range Segments(p) {
		r, err := Render(events, Target{Prefix: "My reply"}, Window)
		if err != nil || r.Reply != "My reply here." || n != 0 {
			t.Fatalf("%v %v", r, err)
		}
		n++
	}
}
