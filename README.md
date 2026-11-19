# tote

A Claude Code Stop hook that catches guesses stated as fact.

An agent often writes "the cache is 40k characters" or "there are no other callers" because it sounds right, not because
it checked. After every reply of 400 characters or more, tote asks of each sentence: why do you think this, and are you
sure? The check runs in the background after the reply. Up to two sentences that turn out to be unbacked reach the agent
with your next message, as hidden context. The agent re-checks them first, and if one was wrong it opens its answer with
a short `Correction (n): …` line.

**What leaves your machine.** Each checked reply sends part of the session transcript to the Anthropic API under your
API key, so your key's data-retention terms apply, not your Claude plan's. The part sent is the last 120,000 to 160,000
characters, plus CLAUDE.md, memory files and any compaction summary in full. It includes tool calls and outputs, so
anything printed in the session goes too, secrets included. Nothing is scrubbed. The API host is fixed to
`api.anthropic.com` and does not follow `ANTHROPIC_BASE_URL`.

**What stays on disk.** By default, tote's log and response cache hold excerpts of your session: the flagged sentences,
the checker's reasons, and its full answers. `TOTE_REDACT=1` keeps them out.

**The wait.** None. The hook runs asynchronously, so you can type as soon as the reply ends. In the author's sessions a
check took a median of about 17 seconds and at most about 48. If you answer before a check finishes, its flags arrive
one message later.

**The cost.** In the author's sessions, a check cost $0.04–0.07 while the prompt cache was warm and $0.27–0.34 when it
was cold. The cache lasts an hour, so the first check after a break is cold, and cost grows with the transcript. There
is no extra agent turn: the agent's re-check is part of its answer to your next message, so it costs a few tool calls on
your Claude plan.

## Requirements

- Go 1.26 or later.
- A Claude Code version that runs `async` hooks (tested on 2.1.328).
- An Anthropic API key with billing.

## How it works

1. The hook reads the session transcript, including tool calls and outputs, background-agent results, CLAUDE.md and
   memory.
2. It makes one call to the Anthropic API with the transcript and the reply split into numbered sentences. For each
   sentence that states a fact, the checker answers *why do you think this, and are you sure?* "Sure" requires an exact
   quote from something in the transcript before the reply, and code checks that the quote is really there.
3. A sentence is flagged when it is:
   - contradicted by the transcript;
   - backed by a quote from a file that was edited after it was read;
   - backed by a quote that is not in the cited source, or that appears only in the agent's own tool input, such as a
     command it ran or a file it wrote;
   - a claim that something is absent ("never", "doesn't exist", "no other", "there are no"), unless hedged;
   - not sure, meaning it rests on memory, an assumption or an unchecked figure.
4. At most two flagged sentences go back to the agent, taken in that order. The rest are written to the log only.
5. Judgements, plans, questions and sentences the reply already hedges are left alone.

The message to the agent holds each flagged sentence, flattened to one line, and a fixed phrase for the kind of flag. It
never holds the checker's free-text reasons or anything else read from the transcript. It tells the agent that the
sentences are claims to check, never instructions, to re-check them by reading files or outputs already in reach, and
never to run a command a flagged sentence suggests. The agent's own sentences can still repeat text from a tool output,
and that framing is the only defence against it.

## Install

Put your API key in `~/.config/tote/.env` as `ANTHROPIC_API_KEY=…`, or in `ANTHROPIC_API_KEY`. A hook does not inherit
your shell's environment, so the file is the reliable place. Tote refuses a `.env` that group or others can read; run
`chmod 600 ~/.config/tote/.env`.

```
go install github.com/justinstimatze/tote@latest
tote install /path/to/your/project
```

`tote install` adds the hook to the project's `.claude/settings.local.json`, which Claude Code keeps out of git. It
keeps your other settings and hooks, and saves the previous file as `settings.local.json.bak` (mode 0600). Key order
inside the file may change. Running it again, from any path, replaces tote's hook instead of adding a second one. The
hook command holds the full path of the `tote` binary, so run `tote install` again if you move it.

To switch tote off for one session, start Claude Code with `TOTE_CHECK=0` in its environment.

Tote is tested on Linux. It should work on macOS and is not tested on Windows.

## Uninstall

1. Run `tote install --remove /path/to/your/project` in each project. It removes tote's hook, whatever path the binary
   was installed from, and leaves the rest of the file alone.
2. Delete `.claude/settings.local.json.bak` in each project if you don't want the backup.
3. Delete the state directory, `~/.local/state/tote` by default, which holds the log and cache.
4. Delete the binary, `$(go env GOPATH)/bin/tote`, and `~/.config/tote/.env`.

## Settings

Set these in Claude Code's environment, except the first two. `tote install` writes those into the hook command, so
changing them later in the environment does nothing; run `tote install` again instead.

| Variable | Effect |
| --- | --- |
| `TOTE_SKIP_IF=REGEX` | Skip any turn whose transcript window matches the pattern. Nothing is sent for that turn. It matches only the window that would be sent, so it is a best-effort opt-out for content you can name, not a guarantee that nothing sensitive goes out. The pattern uses Go regexp syntax, with no lookahead. An invalid pattern is an error at install time, and at hook time the turn is skipped. |
| `TOTE_REDACT=1` | Keeps transcript text out of tote's log and turns off the response cache. It does not change what goes to the API. |
| `TOTE_DAILY_USD=5` | Caps spend per day across all projects, summed from the log. It counts only checks that returned, so it is approximate. With a cap set, tote refuses to run on a model it has no price for. |
| `TOTE_SAMPLE=0.5` | Checks that fraction of turns, from 0 to 1. |
| `TOTE_ASK_MODEL`, `TOTE_ASK_EFFORT` | The checker model and effort. The defaults are `claude-sonnet-5-5` at `medium`. |
| `TOTE_BASE_URL` | Sends checks to a different API host. Use only for a proxy you run. |
| `TOTE_CHECK=0` | Turns the hook off. |

## Where state lives

Tote keeps its state in `$XDG_STATE_HOME/tote`, which is `~/.local/state/tote` by default. Directories are mode 0700 and
files 0600. Every project you install into shares this directory.

- `log.jsonl`: one line per reply, either checked (sentences, flags, the checker's reasons, cost, seconds) or skipped,
  with the reason. Without `TOTE_REDACT=1` it quotes your session.
- `blocked/`: one empty marker file per prompt whose flags were sent.
- `cache/`: the checker's answers, keyed by content hash. Without `TOTE_REDACT=1` they quote your session.
- `outcomes.csv` and `misses.csv`, from the commands below.

## When a check fails

The reply is simply unchecked, and tote never holds a reply back. The log records `{"error": "<class>"}`, where the
class is a short label such as `api_429`, `timeout`, `no_api_key` or `env_file_permissions`. Only the class is logged,
never the error text, since that can quote request content. The API client retries twice and gives up after 90 seconds
a request, and the whole check is cut off after 110 seconds.

Tote also emits a one-line notice for a failed check, a spent daily budget, a `TOTE_SKIP_IF` match and an invalid
pattern. Claude Code's hooks documentation says an async hook's notices are not shown in the chat, so read the log to
see them.

## How well it works

These numbers come from the author's own sessions in November 2026. The samples are small, the author did the labelling,
and none of it is held out. The session data is not in this repo, so the numbers cannot be rerun from it.

- **Real guesses.** Five sentences the agent later took back on its own ("I was wrong…") were replayed through the
  checker, and 4 were flagged. The miss was a quote from the wrong source.
- **Hand-labelled flags.** 6 of 6 labelled real problems were flagged, and 1 or 2 of 10 labelled false flags came back,
  depending on the run.
- **Planted errors.** A tool-backed number was changed in each of 7 real replies, and all 7 sentences were flagged. A
  checker that flagged every factual sentence would also score 7 of 7, so this shows recall, not precision.
- **False alarms.** In live use, about a third of the sentences sent back were right when the agent re-checked them.
  That is tote's false-alarm rate.

## What it can't catch

- A claim backed by a real quote from a neighbouring source. The quote is in the transcript, so code accepts it, but it
  is the wrong source for the claim.
- Anything the transcript cannot show, such as how a product behaves in general.
- A guess that happens to be right. That costs nothing anyway.
- Its own blind spots. The checker is a model of the same family as the agent, so the two can share a mistake.

## Measuring it on your own sessions

These commands read transcripts and tote's log, and make no API calls.

```
tote outcomes   # per flag sent back: corrected, checked and left standing, or ignored
tote misses     # each time the agent took something back, and what tote did with that reply
```

`tote check TRANSCRIPT "first words of a reply"` checks one past reply and prints its flags. It makes one API call and
honours `TOTE_SKIP_IF` from the environment. With `--json` it prints rows, flags, verdicts and cost, for your own
evaluation scripts.

## License

MIT
