# tote

A Claude Code Stop hook that catches guesses stated as fact.

An agent often writes "the cache is 40k characters" or "nothing else calls this" because it sounds right, not because
it checked. After every reply of 400 characters or more, tote asks of each sentence: why do you think this, and are you
sure? Sentences that turn out to be unbacked go back to the agent, which checks them with a tool and corrects them.
You see only a short `Correction (n): …` line when a sentence was wrong, or the word `Checked.` when every flagged
sentence held.

**What leaves your machine.** Each checked reply sends the recent part of the session transcript to the Anthropic API,
up to 120,000 characters. That includes tool outputs, so anything printed in the session goes too, along with CLAUDE.md
and memory. If that is a problem for a project, see `TOTE_SKIP_IF` below.

**The wait.** The check adds a median of about 17 seconds to a checked reply, and up to about 48 seconds. Those are
measured over 60 checks.

**The cost.** Per checked reply: $0.04–0.07 while the prompt cache is warm, $0.27–0.34 when it is cold. When a reply is
sent back, the agent's corrections turn costs about $0.42 (the mean over 24 such turns). For a heavy day of 100 checked
replies, 80 warm and 20 cold, the checks come to about $9–12. If one reply in five is sent back, that adds about $8.
Both numbers are estimates built from the per-reply costs. Neither is a measured day.

## How it works

1. The hook reads the session transcript, including tool calls and outputs, background-agent results, CLAUDE.md and
   memory.
2. It makes one call to the Anthropic API with the transcript and the reply split into numbered sentences. For each
   sentence that states a fact, the checker answers *why do you think this, and are you sure?* "Sure" requires an exact
   quote from something in the transcript before the reply, and code checks that the quote is really there.
3. It sends back to the agent, as hidden context, each sentence that is:
   - not sure, meaning it rests on memory, an assumption or an unchecked figure;
   - contradicted by the transcript;
   - a claim that something is absent or the only one ("never", "doesn't exist", "no other", "the only"), unless
     hedged;
   - backed by a quote that is not in the cited source, or that comes from a file edited after it was read.
4. Judgements, plans, questions and sentences the reply already hedges are left alone.

The message to the agent holds the sentence, which is the agent's own words, and a fixed phrase for the kind of flag.
It never holds the checker's free-text reasons, so text the checker read in the transcript cannot steer the agent. The
message tells the agent to re-check by reading files or outputs already in reach, and never to run a command that a
flag suggests. The checker's reasons go to the local log only.

The agent answers once. A turn is sent back at most once, tracked by a marker file per prompt, and the corrections turn
is not checked again.

## Install

You need Go and an Anthropic API key with billing. Put the key in `ANTHROPIC_API_KEY` or in `~/.config/tote/.env` as
`ANTHROPIC_API_KEY=…`. A Stop hook does not inherit your shell's environment, so the file is the reliable place. Tote
refuses a `.env` that group or others can read; run `chmod 600 ~/.config/tote/.env`.

```
go install github.com/justinstimatze/tote@latest
tote install /path/to/your/project
```

`tote install` adds the hook to the project's `.claude/settings.local.json`, which is gitignored. It keeps your other
settings and hooks, and writes a backup first. Key order inside that file may change. Running it again replaces tote's
hook and does not add a second copy. The hook command holds the full path of the `tote` binary, so run `tote install`
again if you move it.

To remove tote from a project, run `tote install --remove /path/to/your/project`. It removes only the hook it installed
for this binary. To switch tote off for one session, start Claude Code with `TOTE_CHECK=0`
in its environment.

Tote is tested on Linux. It should work on macOS and is not tested on Windows.

## Settings

Set these in Claude Code's environment, except the first two, which `tote install` writes into the hook command, so
run it again to change them.

| Variable | Effect |
| --- | --- |
| `TOTE_SKIP_IF=REGEX` | Skip any turn whose transcript window matches the pattern, for example the command that reads production data. It matches the window that would be sent (the last 120,000 characters), not the whole transcript. Nothing is sent for that turn. The pattern uses Go regexp syntax, with no lookahead. An invalid pattern is an error at install time, and at hook time the turn is skipped and nothing is sent. |
| `TOTE_REDACT=1` | Keeps transcript text out of tote's local log and turns off the on-disk response cache. It does not change what goes to the API: the transcript is still sent. |
| `TOTE_DAILY_USD=5` | Caps spend per day across all projects, summed from the log. With a cap set, tote refuses to run on a model it has no price for. |
| `TOTE_SAMPLE=0.5` | Checks that fraction of turns. |
| `TOTE_ASK_MODEL`, `TOTE_ASK_EFFORT` | The checker model and effort. The defaults are `claude-sonnet-5-5` at `medium`. |
| `TOTE_CHECK=0` | Turns the hook off. |

## Where state lives

Tote keeps its state in `$XDG_STATE_HOME/tote`, which is `~/.local/state/tote` by default. Directories are mode 0700 and
files 0600.

- `log.jsonl`: one line per reply, either checked (sentences, flags, cost, seconds) or skipped, with the reason.
- `blocked/`: one empty marker file per prompt that was sent back.
- `cache/`: the checker's responses, keyed by content hash.
- `outcomes.csv` and `misses.csv`, from the commands below.

Every project you install into shares this directory.

## When a check fails

The reply goes out unchecked and tote never holds it back. You see one line: `tote: the check failed (<class>), so
this reply is unchecked`. The class is a short label such as `api_429`, `timeout`, `no_api_key` or
`env_file_permissions`. Only the class is logged, never the error text, since that can quote request content. The API
client retries twice and gives up after about 90 seconds a request. A check still running at the hook's 120-second
timeout is stopped without a notice.

## How well it works

This is a development set, from the author's own sessions. The samples are small and none of it is held out. The
session data is not included in this repo.

- **Real guesses.** Five sentences the agent later took back on its own ("I was wrong…") were replayed through the
  checker, and 4 were sent back. The miss was a quote taken from the wrong source: a game's script cited for a claim
  about the TV show.
- **Hand-labelled flags.** 6 of 6 labelled real problems were flagged, and 1 or 2 of 10 labelled false flags came back,
  depending on the run.
- **Planted errors.** A tool-backed number was changed in each of 7 real replies. All 7 sentences were flagged, and in
  5 of them the checker named the planted number. A baseline that flags every factual sentence would also flag all 7,
  so this test shows recall, not precision.
- **Noise.** On 7 unchanged replies, about 1.4 of 10 sentences per reply were flagged. In live use, about a third of
  the sentences sent back held when the agent re-checked them, and each of those cost a corrections turn.

## What it can't catch

- A claim backed by a real quote from a neighbouring source. The quote is in the transcript, so code accepts it, but it
  is the wrong source for the claim.
- Anything the transcript cannot show, such as how a product behaves in general.
- A guess that happens to be right. That costs nothing anyway.
- Its own blind spots. The checker is a model of the same family as the agent, so the two can share a mistake.

## Measuring it on your own sessions

These commands read transcripts and tote's log, and make no API calls.

```
tote outcomes   # per flag: corrected, checked and left standing, or ignored; turn cost
tote misses     # each time the agent took something back, and what tote did with that reply
```

`tote check TRANSCRIPT "first words of a reply"` checks one past reply and prints its flags. It makes one API call.
With `--json` it prints rows, flags, verdicts and cost, for your own evaluation scripts.

## License

MIT
