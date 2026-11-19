"""What happened to each flagged sentence after tote sent a reply back: read from the transcripts, no model call.

For every block (a "Stop hook feedback: tote..." message in a session transcript), take the flagged sentences named in
it, the agent's turn that answered it, and what that turn cost. Each flag gets one outcome:
  corrected     the answer has a "Correction (n):" line for it (or, for unnumbered corrections, one that matches it)
  checked       a tool call followed and no correction names it: re-checked and left standing
  ignored       no tool call and no correction
Older blocks, from before corrections-only answers, are scored by whether the sentence still appears as written:
  changed_after_check, confirmed_after_check, dropped_or_hedged, ignored.

Each correction also gets a type, by code on the flag and the correction:
  existence   the correction negates what the sentence asserted ("doesn't", "isn't", "no longer")
  value       a number, path or name in the correction is not in the sentence
  scope       the sentence had every/all/only/never/always/can and the correction drops it
  other

  python -m tote.outcomes     prints the tally and writes data/check/outcomes.csv
"""

import csv
import glob
import json
import os
import re
from collections import Counter
from pathlib import Path

from tote import ask, transcript

CHECK = Path(__file__).resolve().parents[1] / "data" / "check"
BLOCK = re.compile(r"^Stop hook feedback:\s*\ntote(?: ledger)?: (?:\d+ (?:claim|sentence)|re-check these sentences)")
FLAG = re.compile(r'^(?:- |\d+\. )(?:row \d+, )?"(.+?)"[:\s(]', re.M)  # the quoted sentence or span of each flag line
NUMBERED = re.compile(r"^\d+\. ", re.M)
CORRECTION = re.compile(r"^Correction(?: \((\d+)\))?:\s*(.+)$", re.M)
REBUT = re.compile(r"false flag|wrong flag|flags? (?:is|are|was|were) wrong|flag (?:is|was) false|backing is there|"
                   r"(?:is|was) backed|not a guess", re.I)
NEG = re.compile(r"\b(?:doesn't|does not|isn't|is not|wasn't|was not|aren't|don't|do not|didn't|did not|can't|cannot|"
                 r"no longer|never|not yet|nothing|none|no \w+)\b", re.I)
SCOPE = re.compile(r"\b(?:every|all|only|never|always|each|any|can)\b", re.I)


def turn_cost(usages: dict) -> float:
    """Dollars for the agent's API calls in a turn, one usage per message id (the transcript repeats a message per
    content block). Cache writes bill at 2x for the 1-hour TTL and 1.25x for 5 minutes, reads at 0.1x."""
    total = 0.0
    for model, u in usages.values():
        pin, pout = ask.PRICES.get(model, (0.0, 0.0))
        cc = u.get("cache_creation") or {}
        w1, w5 = cc.get("ephemeral_1h_input_tokens", 0), cc.get("ephemeral_5m_input_tokens", 0)
        w5 += max(0, (u.get("cache_creation_input_tokens") or 0) - w1 - w5)
        total += ((u.get("input_tokens") or 0) * pin + w1 * pin * 2 + w5 * pin * 1.25 +
                  (u.get("cache_read_input_tokens") or 0) * pin * 0.1 + (u.get("output_tokens") or 0) * pout) / 1e6
    return total


def blocks(path: str):
    """(block text, tool calls, the agent's last reply text, turn cost in dollars) for each block, streamed.
    A turn often answers in text first and checks after ("I'm re-running both"), so the whole turn is read."""
    cur = None  # [block text, tool calls, last reply text, {message id: (model, usage)}]

    def done(c):
        return c[0], c[1], c[2], turn_cost(c[3])

    with open(path, "rb") as f:
        for line in f:
            if cur is None and b"Stop hook feedback" not in line and b"hookAdditionalContext" not in line:
                continue
            try:
                e = json.loads(line)
            except ValueError:
                continue
            m = e.get("message") if isinstance(e.get("message"), dict) else {}
            c = m.get("content")
            if e.get("subtype") == "stop_hook_summary":  # silent send-back: the hook's additionalContext
                ctx = [x for x in e.get("hookAdditionalContext") or [] if x.startswith("tote")]
                if ctx:
                    if cur and cur[2]:
                        yield done(cur)
                    cur = ["Stop hook feedback:\n" + ctx[0], 0, "", {}]
                continue
            if e.get("type") == "user" and isinstance(c, str):
                if cur and e.get("isMeta"):  # Claude Code's nudge after an empty turn belongs to the same turn
                    continue
                if cur and cur[2]:
                    yield done(cur)
                cur = [c, 0, "", {}] if BLOCK.match(c) else None  # any other user message ends the turn
                continue
            if cur is None or e.get("type") != "assistant":
                continue
            if m.get("id") and m.get("usage"):
                cur[3][m["id"]] = (m.get("model", ""), m["usage"])
            for b in c if isinstance(c, list) else []:
                if b.get("type") == "tool_use":
                    cur[1] += 1
                elif b.get("type") == "text" and b["text"].strip():
                    cur[2] = b["text"]
        if cur and cur[2]:
            yield done(cur)


def correction_type(flag: str, correction: str) -> str:
    if NEG.search(correction) and not NEG.search(flag):
        return "existence"
    fn, fi = transcript.nums(flag), transcript.identifiers(flag)[0]
    cn, ci = transcript.nums(correction), transcript.identifiers(correction)[0]
    if cn - fn or ci - fi:  # the correction body has its "(n)" stripped, so even a single digit is a value
        return "value"
    if (words := {w.lower() for w in SCOPE.findall(flag)}) and not words <= {w.lower() for w in SCOPE.findall(correction)}:
        return "scope"
    return "other"


def overlap(a: str, b: str) -> int:
    return len(set(re.findall(r"\w{4,}", a.lower())) & set(re.findall(r"\w{4,}", b.lower())))


def score(text: str, calls: int, reply: str) -> list[dict]:
    """One row per flag in a block: its outcome, and the correction and its type when there is one."""
    flags = FLAG.findall(text)
    if not flags:
        return []
    found = CORRECTION.findall(reply)
    if not found and not reply.strip().startswith("Checked") and "re-check these sentences" not in text:
        # an older block, answered by rewriting the reply: score by whether the sentence survived
        r = transcript.norm(reply)
        return [{"flag": f, "outcome": ("confirmed_after_check" if transcript.norm(f) in r else "changed_after_check") if calls
                 else ("ignored" if transcript.norm(f) in r else "dropped_or_hedged"), "correction": "", "type": ""}
                for f in flags]
    fixes = {}
    for n, body in found:
        i = int(n) - 1 if n else max(range(len(flags)), key=lambda k: overlap(flags[k], body))
        if 0 <= i < len(flags):
            fixes.setdefault(i, body.strip())
    return [{"flag": f, "outcome": "corrected" if i in fixes else ("checked" if calls else "ignored"),
             "correction": fixes.get(i, ""), "type": correction_type(f, fixes[i]) if i in fixes else ""}
            for i, f in enumerate(flags)]


def main():
    sessions = {json.loads(l).get("session") for l in open(CHECK / "log.jsonl") if '"flags"' in l}
    rows, turns, costs = [], Counter(), []
    for s in sorted(x for x in sessions if x):
        for path in glob.glob(os.path.expanduser(f"~/.claude/projects/*/{s}.jsonl")):
            for text, calls, reply, cost in blocks(path):
                new = "re-check these sentences" in text
                turns["checked" if calls else "no_tool_call"] += 1
                turns["rebutted"] += bool(REBUT.search(reply))
                if new:
                    costs.append(cost)
                for r in score(text, calls, reply):
                    rows.append({"session": s, "calls": calls, "turn_cost": round(cost, 4), **r})
    fields = ["session", "flag", "calls", "outcome", "type", "correction", "turn_cost"]
    with open(CHECK / "outcomes.csv", "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        w.writerows(rows)
    n = Counter(r["outcome"] for r in rows)
    print(f"{sum(turns[k] for k in ('checked', 'no_tool_call'))} blocked turns: {turns['checked']} with a tool call, "
          f"{turns['no_tool_call']} without; {turns['rebutted']} say a flag was wrong")
    print(f"{len(costs)} corrections-only turns cost ${sum(costs):.2f} to answer (${sum(costs) / max(1, len(costs)):.3f} a turn)")
    for k, v in n.most_common():
        print(f"  {k:22} {v:4}  ({v / max(1, len(rows)):.0%})")
    t = Counter(r["type"] for r in rows if r["type"])
    if t:
        print("  correction types: " + ", ".join(f"{k} {v}" for k, v in t.most_common()))
    print(f"{len(rows)} flags from {len({r['session'] for r in rows})} sessions -> {CHECK / 'outcomes.csv'}")


if __name__ == "__main__":
    main()
