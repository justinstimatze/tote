# tote

A Claude Code Stop hook that catches guesses stated as fact.

An agent often writes "the cache is 40k characters" or "nothing else calls this" because it sounds right, not because
it checked. It doesn't know these are guesses until someone asks "are you sure?". Tote asks for you, after every reply.
Each sentence that turns out to be unbacked goes back to the agent, which checks it with a tool and corrects it. You
never see the exchange. You see only a short `Correction (n): …` line when a sentence was wrong, or the word `Checked.`
when every flagged sentence held.

## How it works

When the agent finishes a reply of 400 characters or more, the hook:

1. Reads the session transcript, including tool calls and outputs, background-agent results, CLAUDE.md and memory.
2. Makes one call to Claude Sonnet 5.5 with the transcript and the reply split into numbered sentences. For each
   sentence that states a fact, the call answers *why do you think this, and are you sure?* "Sure" requires an exact
   quote from something in the transcript before the reply, and code checks that the quote is really there.
3. Sends back to the agent, as hidden context, the sentences that are:
   - not sure, meaning they rest on memory, an assumption or an unchecked figure;
   - contradicted by the transcript;
   - claims that something is absent ("never", "doesn't exist", "no other"), unless they're hedged;
   - backed by a quote that isn't in the cited source, or that comes from a file edited after it was read.
4. Judgements, plans, questions and sentences the reply already hedges are left alone.

The agent answers once. Tote doesn't check that answer, so a reply is sent back at most once.

## Install

You need:
- [uv](https://docs.astral.sh/uv/), which supplies Python 3.12 or later and the one dependency (`anthropic`);
- `jq` and `bash`, for the installer. It's tested on Linux, should work on macOS, and isn't tested on Windows;
- an Anthropic API key with billing. Put it in `ANTHROPIC_API_KEY` or in `~/.config/tote/.env` as
  `ANTHROPIC_API_KEY=…`. A Stop hook doesn't inherit your shell's environment, so the file is the reliable place.

```
git clone <this repo> ~/tote
~/tote/deploy/claude-hooks/install.sh /path/to/your/project
```

`install.sh` adds the hook to the project's `.claude/settings.local.json`, which is gitignored. It keeps your other
settings and hooks, and writes a backup first. Running it again replaces tote's hook and doesn't add a second copy.
With no argument it installs into the tote checkout itself.

The hook runs tote from where you cloned it. If you move the clone, run `install.sh` again for each project. To take
tote out of a project, run `install.sh --remove /path/to/your/project`. To switch it off for one session, start
Claude Code with `TOTE_CHECK=0` in its environment.

To see that it's working, look at `data/check/log.jsonl` in the tote checkout. Each reply gets a line there, either
checked, with its flags and cost, or skipped, with the reason.

If a check fails, the reply goes out unchecked and tote never holds it back. A missing API key or an API error shows
a one-line notice saying the reply is unchecked. A check still running at the hook's 300-second timeout is stopped
without that notice.

## What it sends, and what it costs

Each checked reply sends the most recent part of the transcript (up to 120,000 characters), plus CLAUDE.md and
memory, to the Anthropic API.

- `TOTE_SKIP_IF=REGEX install.sh DIR` skips any turn whose transcript matches the pattern, for example the command
  that reads production data. Nothing is sent for that turn.
- `TOTE_REDACT=1 install.sh DIR` keeps transcript text out of tote's log and turns off the on-disk response cache.

Both settings are written into the project's hook command, so to change either one, run the installer again. The log
(`data/check/log.jsonl`) and the response cache (`data/ask/cache/`) live in the tote checkout, and every project you
install into shares them.

The transcript goes to the API as a prompt-cached prefix, so consecutive replies in a session mostly re-read it.
Measured in development sessions: $0.04–0.07 per checked reply while the cache is warm, and $0.27–0.34 when it's cold.
The cache goes cold about once per 40,000 characters of new transcript, or after an hour idle. When a reply is sent
back, the agent's corrections turn costs about $0.42 on Opus 5.5 (the mean over 24 such turns).

Optional limits: `TOTE_DAILY_USD` caps spend per day across all projects, and `TOTE_SAMPLE=0.5` checks half of all turns. Both are unset
by default. `TOTE_ASK_MODEL` and `TOTE_ASK_EFFORT` change the checker model, which defaults to `claude-sonnet-5-5`
at `medium` effort.

## How well it works

All of these numbers come from the author's own Claude Code sessions. The samples are small, and the session data
isn't included in this repo.

- **Real guesses.** Five sentences the agent later took back on its own ("I was wrong…") were replayed through the
  checker, and 4 were sent back. The miss was a quote taken from the wrong source: a game's script cited for a claim
  about the TV show.
- **Hand-labelled flags.** From an earlier version's flags, 6 of 6 labelled real problems were caught, and 1 of 10
  labelled false flags came back.
- **Planted errors.** A tool-backed number was changed in each of 7 real replies, and all 7 sentences were sent back.
  In 5 of them the checker named the planted number as wrong.
- **Noise.** Checking unchanged replies, it sends back about 1.4 sentences per 10-sentence reply. In live use, about a
  third of the sentences sent back turned out to hold, and each of those cost a corrections turn.

What it can't catch: a guess that happens to be right, which costs nothing anyway; a claim backed by a real quote
from the wrong source; anything the transcript can't show, such as how a product behaves in general. The checker is
a model, so it has its own blind spots.

## Measuring it on your own sessions

Both commands read transcripts and tote's log, and make no API calls.

```
uv run --project ~/tote python -m tote.outcomes   # per flag: corrected, checked and left standing, or ignored; turn cost
uv run --project ~/tote python -m tote.misses     # each time the agent took something back, and what tote did with that reply
```

`python -m tote.ask <transcript.jsonl> "<first words of a reply>"` checks one past reply and prints its flags.

Every run is logged to `data/check/log.jsonl` with the sentences, flags, cost and time taken.

## History

Tote started as an experiment in having agents bet on their own claims and on each other's, to learn how far to trust
a model's stated confidence. The question narrowed to whether an agent can tell when it's guessing. It can, but
mostly only when asked, and this hook does the asking.

## License

MIT
