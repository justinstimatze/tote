"""Guesses tote let through: the user asked for a double-check and the agent then retracted something.

For each user prompt that asks for a re-check ("are you sure", "double check", "that's wrong"), read the agent's
answer for a retraction, and join the reply being questioned to the hook log entry for it:
  passed        checked, nothing flagged: a retraction here is a miss
  flagged       checked and sent back, and the user still had to ask
  skipped: <reason>   not checked (short reply, budget spent, TOTE_SKIP_IF, ...)
  no hook run   no log entry between that reply and the prompt (tote not installed, or the hook did not fire)
The rate of double-check prompts per 100 replies, with and without tote, is the number to compare.

  python -m tote.misses                         sessions tote has logged
  python -m tote.misses --project NAME [--until 2026-11-18]   every session of a project dir, e.g. a pre-tote baseline
Writes data/check/misses.csv. Prompt text is cut to 120 characters and stays on this machine.
"""

import argparse
import csv
import glob
import json
import os
import re
from datetime import datetime
from pathlib import Path

CHECK = Path(__file__).resolve().parents[1] / "data" / "check"
# a prompt that questions the reply just given: "double check" alone is often an instruction for new work, so it
# counts only when it points back ("double check that", "double check your numbers") or the prompt is a question
BROAD = re.compile(r"\b(?:double[- ]?check|are you sure|you sure\?|verify|that'?s (?:wrong|not right|incorrect)|check again|"
                   r"is that (?:right|true|correct)|did you (?:check|verify|actually))", re.I)
ASK = re.compile(r"\b(?:are you sure|you sure\b|that'?s (?:wrong|not right|incorrect|not true)|is that (?:right|true|correct)|"
                 r"did you (?:check|verify|actually)|double[- ]?check (?:that|this|it|your|those|these|again)|"
                 r"check (?:that|this) again|really\?)", re.I)
AUTOMATED = ("Overnight", "Here is a note offered by a side agent", "[Request interrupted", "Caveat:", "<")
MAX_PROMPT = 400  # a long prompt is a brief, not a question about the last reply
# the agent taking back something it said: the miss itself, whatever the prompt that drew it out
RETRACT = re.compile(r"^Correction\b|\bI was wrong\b|\bI misread\b|\bI misspoke\b|\b(?:that|which) was wrong\b|"
                     r"\bI stand corrected\b|\bI overstated\b|\bwithout checking\b(?! (?:back|with|out|first|in\b))|\bwrong (?:earlier|last turn|before)\b|"
                     r"\bI said .{0,80}\b(?:but|that was)\b.{0,40}\b(?:wrong|not)\b", re.I | re.M)


def when(ts: str) -> datetime | None:
    try:
        return datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except (ValueError, AttributeError):
        try:
            return datetime.strptime(ts, "%Y-%m-%dT%H:%M:%S%z")
        except (ValueError, TypeError):
            return None


def hook_log() -> dict:
    """session -> [(time, entry)] from the hook log, in order."""
    out = {}
    p = CHECK / "log.jsonl"
    if p.exists():
        for line in open(p):
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if e.get("session") and (t := when(e.get("time", ""))):
                out.setdefault(e["session"], []).append((t, e))
    return out


def status(entries: list, after: datetime | None, before: datetime) -> str:
    hits = [e for t, e in entries if (after is None or t >= after) and t <= before]
    if not hits:
        return "no hook run"
    e = hits[-1]
    if e.get("skip"):
        return f"skipped: {e['skip']}"
    return "flagged" if e.get("flagged") or e.get("flags") else "passed"


def turns(path: str):
    """(time of the reply the user answered, that reply's text, the user prompt, its time, the agent's answer text),
    one per real user prompt, streamed."""
    last_reply, last_t, prompt, prompt_t, answer = "", None, None, None, []
    with open(path, "rb") as f:
        for line in f:
            if b'"user"' not in line and b'"assistant"' not in line:
                continue
            try:
                e = json.loads(line)
            except ValueError:
                continue
            m = e.get("message") if isinstance(e.get("message"), dict) else None
            if not m:
                continue
            c = m.get("content")
            if e.get("type") == "user" and isinstance(c, str):
                if c.startswith("<") or c.startswith("Stop hook feedback") or c.startswith("This session is being continued"):
                    continue
                if prompt is not None:
                    yield last_t_prev, reply_prev, prompt, prompt_t, "\n".join(answer)
                reply_prev, last_t_prev = last_reply, last_t
                prompt, prompt_t, answer = c, when(e.get("timestamp", "")), []
                continue
            if e.get("type") == "assistant":
                for b in c if isinstance(c, list) else []:
                    if isinstance(b, dict) and b.get("type") == "text" and b["text"].strip():
                        last_reply, last_t = b["text"], when(e.get("timestamp", ""))
                        if prompt is not None:
                            answer.append(b["text"])
    if prompt is not None:
        yield last_t_prev, reply_prev, prompt, prompt_t, "\n".join(answer)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--project", help="substring of a ~/.claude/projects directory name")
    ap.add_argument("--until", help="only sessions whose prompts come before this date (YYYY-MM-DD)")
    a = ap.parse_args()
    log = hook_log()
    if a.project:
        paths = [p for d in glob.glob(os.path.expanduser("~/.claude/projects/*")) if a.project in os.path.basename(d)
                 for p in glob.glob(f"{d}/*.jsonl")]
    else:
        paths = [p for s in log for p in glob.glob(os.path.expanduser(f"~/.claude/projects/*/{s}.jsonl"))]
    until = when(a.until + "T00:00:00+00:00") if a.until else None
    rows, prompts = [], 0
    for path in paths:
        session = Path(path).stem
        for reply_t, reply, prompt, prompt_t, answer in turns(path):
            if until and prompt_t and prompt_t >= until:
                break
            if prompt.startswith(AUTOMATED):
                continue
            prompts += 1
            hit = RETRACT.search(answer)
            if not BROAD.search(prompt) and not hit:
                continue
            strict = len(prompt) <= MAX_PROMPT and bool(ASK.search(prompt))
            rows.append({"session": session, "time": prompt_t.isoformat() if prompt_t else "",
                         "prompt": " ".join(prompt.split())[:120], "strict": strict, "retracted": bool(hit),
                         "retraction": " ".join(answer[max(0, hit.start() - 40): hit.end() + 120].split()) if hit else "",
                         "hook": status(log.get(session, []), reply_t, prompt_t) if prompt_t else "no hook run",
                         "reply_chars": len(reply)})
    CHECK.mkdir(parents=True, exist_ok=True)
    with open(CHECK / "misses.csv", "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["session", "time", "hook", "strict", "retracted", "reply_chars", "prompt", "retraction"])
        w.writeheader()
        w.writerows(rows)
    asks = len(rows)
    strict, ret = sum(r["strict"] for r in rows), sum(r["retracted"] for r in rows)
    print(f"{prompts} user prompts in {len(paths)} sessions: {ret} answers retract something the agent said "
          f"({100 * ret / max(1, prompts):.1f} per 100 prompts); {strict} prompts question the last reply outright")
    by = {}
    for r in rows:
        k = r["hook"]
        by.setdefault(k, [0, 0])
        by[k][0] += 1
        by[k][1] += r["retracted"]
    for k, (n, ret) in sorted(by.items(), key=lambda x: -x[1][0]):
        print(f"  {k:40} {ret:4} retractions  ({n} rows)")
    print(f"-> {CHECK / 'misses.csv'}")


if __name__ == "__main__":
    main()
