package tote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The API call uses the official SDK. It has typed fields for everything the call needs: output_config with a
// json_schema format and an effort level, and cache_control with ttl "1h" on text blocks (checked in the SDK source,
// message.go: OutputConfigParam, JSONOutputFormatParam, CacheControlEphemeralParam). TestRequestBody pins the wire
// format, including the schema's property order.

// schemaJSON is the checker's output schema. The order of the properties matters: the model writes its fields in
// this order, so "why" comes before "verdict". A Go map would sort the keys, so the schema is kept as JSON text and
// each top-level value is sent as raw JSON.
const schemaJSON = `{"type": "object", "properties": {"rows": {"type": "array", "items": {"type": "object", "properties": {
"row": {"type": "integer"}, "why": {"type": "string"}, "kind": {"type": "string", "enum": ["fact", "judgement"]},
"verdict": {"type": "string", "enum": ["sure", "not_sure", "hedged", "wrong"]}, "quick_check": {"type": "boolean"},
"source": {"type": "string"}, "quote": {"type": "string"}},
"required": ["row", "why", "kind", "verdict", "quick_check", "source", "quote"], "additionalProperties": false}}},
"required": ["rows"], "additionalProperties": false}`

func schema() map[string]any {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(schemaJSON), &top); err != nil {
		panic(err)
	}
	out := make(map[string]any, len(top))
	for k, v := range top {
		out[k] = v
	}
	return out
}

// Prices are dollars per 1M tokens (input, output).
var Prices = map[string][2]float64{
	"claude-haiku-5-5":  {0.10, 0.50},
	"claude-sonnet-5-5": {2.00, 10.00},
	"claude-opus-5-5":   {4.00, 20.00},
}

// Usage is the token counts of one call. In includes the cache-write tokens.
type Usage struct {
	In         int64 `json:"in"`
	Out        int64 `json:"out"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Cost returns the dollars for one call, and false when the model has no price. A 1-hour cache write bills at 2x
// input, a read at 0.1x.
func Cost(model string, u Usage) (float64, bool) {
	p, ok := Prices[model]
	if !ok {
		return 0, false
	}
	w := float64(u.CacheWrite)
	return (float64(u.In)-w)*p[0]/1e6 + w*p[0]*2/1e6 + float64(u.CacheRead)*p[0]*0.1/1e6 + float64(u.Out)*p[1]/1e6, true
}

// Verdict is the checker's answer for one numbered sentence.
type Verdict struct {
	Row        int    `json:"row"`
	Why        string `json:"why"`
	Kind       string `json:"kind"`
	Verdict    string `json:"verdict"`
	QuickCheck bool   `json:"quick_check"`
	Source     string `json:"source"`
	Quote      string `json:"quote"`
}

// Flag kinds. The text sent back to the agent uses only the fixed phrase for a kind, never the model's free text.
const (
	FlagNotSure   = "not_sure"
	FlagWrong     = "wrong"
	FlagAbsent    = "absent"
	FlagQuoteGone = "quote_not_found"
	FlagStale     = "stale"
)

// Flag is a sentence to send back.
type Flag struct {
	Row  int    `json:"row"`
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"` // for FlagStale: the edited file
	Why  string `json:"why"`            // the checker's own words: for the log only, never sent to the agent
}

// Phrase is the fixed text that goes to the agent for this flag.
func (f Flag) Phrase() string {
	switch f.Kind {
	case FlagNotSure:
		return "not sure"
	case FlagWrong:
		return "contradicted by the session"
	case FlagAbsent:
		return "says something is absent"
	case FlagQuoteGone:
		return "quoted backing not found"
	case FlagStale:
		return "quoted output predates a later edit to that file" // never the path: it comes from the transcript
	}
	return f.Kind
}

// Judged is a sentence kept out of what is sent back: a judgement, or a fact nothing quick could re-check.
type Judged struct {
	Row int    `json:"row"`
	Why string `json:"why"`
}

// a sentence saying something is not there: the claims a transcript reader most often wrongly passes. Kept to these
// words: adding "nothing" and "only" sent back sentences whose quote the checker had verified, on the labelled set.
var absence = regexp.MustCompile(`(?i)\b(?:never|nowhere|doesn't exist|does not exist|don't have|do not have|no other|none of|` +
	`there (?:is|are) no|isn't (?:there|in|on)|aren't any|not anywhere)\b`)

// IsAbsence reports whether the sentence claims something is absent.
func IsAbsence(s string) bool { return absence.MatchString(s) }

// EditedAfter returns a file the cited call read (named in its input) that an Edit/Write changed after that call
// and before the reply at position `at`.
func EditedAfter(cid string, at int, tools *Tools) string {
	src := tools.Get(cid)
	if src == nil {
		return ""
	}
	isWrite := func(d map[string]any) bool {
		for _, k := range []string{"old_string", "content", "edits"} {
			if _, ok := d[k]; ok {
				return true
			}
		}
		return false
	}
	var own map[string]any
	// an edit cited for what it did stays true after a later edit; only a read can go stale
	if json.Unmarshal([]byte(orEmptyJSON(src.Input)), &own) == nil && isWrite(own) {
		return ""
	}
	for _, id := range tools.Order {
		s := tools.M[id]
		var d map[string]any
		if s.Input == "" || json.Unmarshal([]byte(s.Input), &d) != nil {
			continue
		}
		fp, _ := d["file_path"].(string)
		if fp != "" && src.Pos < s.Pos && s.Pos < at && isWrite(d) && strings.Contains(src.Input, fp) {
			return fp
		}
	}
	return ""
}

func orEmptyJSON(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

var sourceID = regexp.MustCompile(`^\[?(?:OUTPUT|CALL|CONTEXT)\s+|\]$`)

// Classify turns the checker's verdicts into flags to send back and judged sentences, and checks every quote in code.
func Classify(rows []string, verdicts []Verdict, r *Rendered) (flags []Flag, judged []Judged) {
	at := r.At
	// a quote that code can find is the only thing that makes "sure" count
	checkSure := func(x Verdict) *Flag {
		src := sourceID.ReplaceAllString(strings.TrimSpace(x.Source), "") // "[CALL id]" -> "id"
		c := &Claim{Quote: x.Quote, CallID: src, Backing: "tool_output"}
		if src == "user" {
			c.Backing = "user_message"
		}
		// the session summary carries no id in the transcript, so a quote from it gets cited under some ctx id
		if !Verify(c, at, r.Tools, r.Users, r.Summary) {
			if !Verify(&Claim{Quote: c.Quote, CallID: c.CallID, Backing: "summary"}, at, r.Tools, r.Users, r.Summary) {
				return &Flag{Row: x.Row, Kind: FlagQuoteGone, Why: x.Why}
			}
		} else if f := EditedAfter(firstNonEmpty(c.FallbackCall, src), at, r.Tools); f != "" {
			// anchoring: the quote is real, but the file it came from was edited after it was read
			return &Flag{Row: x.Row, Kind: FlagStale, Path: f, Why: x.Why}
		}
		return nil
	}
	for _, x := range verdicts {
		if x.Row < 1 || x.Row > len(rows) {
			continue
		}
		fact := x.Kind == "fact"
		// seeing part of something never shows the rest lacks it, whatever the checker concluded
		if fact && x.Verdict != "hedged" && IsAbsence(rows[x.Row-1]) {
			flags = append(flags, Flag{Row: x.Row, Kind: FlagAbsent, Why: x.Why})
			continue
		}
		if !x.QuickCheck && !((x.Verdict == "wrong" || x.Verdict == "not_sure") && fact) {
			// a judgement, or a sure fact nothing quick could re-check. A sure fact still has its quote checked
			if fact && x.Verdict == "sure" {
				if f := checkSure(x); f != nil {
					flags = append(flags, *f)
					continue
				}
			}
			judged = append(judged, Judged{x.Row, x.Why})
			continue
		}
		if x.Verdict == "wrong" { // contradicted by the session: sent back whatever its kind
			flags = append(flags, Flag{Row: x.Row, Kind: FlagWrong, Why: x.Why})
			continue
		}
		if !fact { // inference rows are kept out of what is sent back
			judged = append(judged, Judged{x.Row, x.Why})
			continue
		}
		switch x.Verdict {
		case "not_sure":
			flags = append(flags, Flag{Row: x.Row, Kind: FlagNotSure, Why: x.Why})
		case "sure":
			if f := checkSure(x); f != nil {
				flags = append(flags, *f)
			}
		}
	}
	return
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Result is the outcome of one check.
type Result struct {
	Rows      []string
	Flags     []Flag
	Judged    []Judged
	Verdicts  []Verdict
	Cost      float64
	CostKnown bool
	Usage     Usage
	Model     string
}

// Checker makes the API call.
type Checker struct {
	Cfg  *Config
	Opts []option.RequestOption // extra client options; tests point it at a local server
}

// buildBlocks lays out the user message: the stable transcript in cache chunks with the 1-hour mark on the last,
// the tail, and the numbered reply.
func buildBlocks(r *Rendered, numbered string) []anthropic.ContentBlockParamUnion {
	parts := strings.Split(r.Stable, Chunk)
	var blocks []anthropic.ContentBlockParamUnion
	for i, c := range parts {
		text := c
		if i == 0 {
			text = "TRANSCRIPT\n\n" + c
		}
		b := anthropic.TextBlockParam{Text: text}
		if i == len(parts)-1 { // earlier chunks are found by the API's lookback
			b.CacheControl = anthropic.CacheControlEphemeralParam{TTL: anthropic.CacheControlEphemeralTTLTTL1h}
		}
		blocks = append(blocks, anthropic.ContentBlockParamUnion{OfText: &b})
	}
	if r.Tail != "" {
		blocks = append(blocks, anthropic.NewTextBlock(r.Tail))
	}
	return append(blocks, anthropic.NewTextBlock("YOUR REPLY\n\n"+numbered))
}

func buildParams(model, effort string, blocks []anthropic.ContentBlockParamUnion) anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 16000,
		System: []anthropic.TextBlockParam{{
			Text:         System,
			CacheControl: anthropic.CacheControlEphemeralParam{TTL: anthropic.CacheControlEphemeralTTLTTL1h},
		}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(effort),
			Format: anthropic.JSONOutputFormatParam{Schema: schema()},
		},
	}
}

type cached struct {
	Data  struct{ Rows []Verdict } `json:"data"`
	Usage Usage                    `json:"usage"`
}

// CheckError carries a short class for the log and the user's notice. The underlying error is never logged:
// it may quote request content.
type CheckError struct {
	Class string
	Err   error
}

func (e *CheckError) Error() string {
	if e.Err != nil {
		return e.Class + ": " + e.Err.Error()
	}
	return e.Class
}
func (e *CheckError) Unwrap() error { return e.Err }

// ErrorClass returns a short label for an error, with no free text from the error.
func ErrorClass(err error) string {
	var ce *CheckError
	if errors.As(err, &ce) {
		return ce.Class
	}
	var ae *anthropic.Error
	switch {
	case errors.As(err, &ae):
		return fmt.Sprintf("api_%d", ae.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "timeout"
		}
		return "network"
	}
	return "error"
}

// Check asks the checker about the rendered reply: one call, cached on disk by content hash unless redacting.
func (c *Checker) Check(ctx context.Context, r *Rendered) (*Result, error) {
	cfg := c.Cfg
	rows := SplitRows(r.Reply)
	if len(rows) == 0 { // all code or table rules: nothing to ask about, so no call
		return &Result{Model: cfg.Model, CostKnown: true}, nil
	}
	var nb strings.Builder
	for i, row := range rows {
		if i > 0 {
			nb.WriteString("\n")
		}
		fmt.Fprintf(&nb, "%d. %s", i+1, row)
	}
	numbered := nb.String()
	keyBytes, _ := json.Marshal([]string{cfg.Model, cfg.Effort, System, r.Stable, r.Tail, numbered})
	sum := sha256.Sum256(keyBytes)
	path := filepath.Join(cfg.StateDir, "cache", hex.EncodeToString(sum[:])+".json")

	var cc cached
	hit := false
	if !cfg.Redact {
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &cc) == nil {
			hit = true
		}
	}
	if !hit {
		key, err := cfg.APIKey()
		if err != nil {
			return nil, err
		}
		// the base URL is pinned: the SDK otherwise honours ANTHROPIC_BASE_URL, which a project's settings can set,
		// and that would send the transcript and the key to whatever host it names
		base := "https://api.anthropic.com/"
		if b := os.Getenv("TOTE_BASE_URL"); b != "" { // tote's own variable, set on purpose, never inherited by accident
			base = b
		}
		opts := append([]option.RequestOption{option.WithAPIKey(key), option.WithBaseURL(base),
			option.WithMaxRetries(2),
			option.WithRequestTimeout(90 * time.Second)}, c.Opts...)
		client := anthropic.NewClient(opts...)
		msg, err := client.Messages.New(ctx, buildParams(cfg.Model, cfg.Effort, buildBlocks(r, numbered)))
		if err != nil {
			return nil, err
		}
		var text string
		for _, b := range msg.Content {
			if b.Type == "text" {
				text = b.Text
				break
			}
		}
		var out struct{ Rows []Verdict }
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, &CheckError{Class: "bad_output", Err: err}
		}
		u := msg.Usage
		cc = cached{Usage: Usage{In: u.InputTokens + u.CacheCreationInputTokens, Out: u.OutputTokens,
			CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}}
		cc.Data.Rows = out.Rows
		if !cfg.Redact {
			if b, err := json.Marshal(cc); err == nil {
				_ = writeFile0600(path, b)
			}
		}
	}
	flags, judged := Classify(rows, cc.Data.Rows, r)
	cost, known := Cost(cfg.Model, cc.Usage)
	if hit { // nothing was billed, so nothing counts toward TOTE_DAILY_USD
		cost = 0
	}
	return &Result{Rows: rows, Flags: flags, Judged: judged, Verdicts: cc.Data.Rows, Cost: cost, CostKnown: known,
		Usage: cc.Usage, Model: cfg.Model}, nil
}
