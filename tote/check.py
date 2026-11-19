"""Stop hook: check the reply the user is about to read, and send the sentences that need it back to the agent, once.

tote.ask asks of each sentence "why do you think this, and are you sure?". Sentences that are not sure, contradicted,
claim an absence, or cite a quote that is not in the session go back to the agent as hidden context (the user sees no
message): it re-checks them with a tool and replies with "Correction (n): ..." lines, or "Checked.". A turn is sent
back at most once (by prompt_id, tracked in data/check/blocked.txt), and the corrections turn is not checked again.

Every run is logged to data/check/log.jsonl: sentences, flags, cost and wall time.

Wire as a Stop hook: uv run --project <tote> python -m tote.check   (stdin: the hook's JSON). TOTE_CHECK=0 disables it.
"""

import hashlib
import json
import os
import re
import sys
import time
from pathlib import Path

from tote import ask, transcript

LOG = Path(__file__).resolve().parents[1] / "data" / "check" / "log.jsonl"
BLOCKED = LOG.parent / "blocked.txt"  # session:prompt_id of turns already sent back once
WINDOW = 120_000  # transcript characters shown to the checker, besides instructions and memory (~30k tokens)
MIN_CHARS = 400  # shorter replies rarely carry checkable claims
# optional spend controls: TOTE_DAILY_USD caps what the hook spends per local day, summed from this log; TOTE_SAMPLE
# checks that fraction of turns, chosen by a hash of the turn
DAILY_USD = float(os.environ.get("TOTE_DAILY_USD") or "inf")  # no cap unless set
SAMPLE = float(os.environ.get("TOTE_SAMPLE", "1"))
def spent_today() -> float:
    if not LOG.exists():
        return 0.0
    today, total = time.strftime("%Y-%m-%d"), 0.0
    with open(LOG) as f:
        for line in f:
            if line.startswith('{"time": "' + today):
                total += json.loads(line).get("cost", 0)
    return total


def sampled(turn: str) -> bool:
    return SAMPLE >= 1 or int(hashlib.sha256(turn.encode()).hexdigest()[:8], 16) / 0xFFFFFFFF < SAMPLE


def final_reply(path: str) -> str:
    """The last assistant text block of the current turn."""
    for _, line in transcript._backward(path):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if e.get("type") == "assistant":
            texts = [b["text"] for b in e["message"].get("content", []) if b.get("type") == "text"]
            if texts:
                return texts[-1]
        if e.get("type") == "user" and isinstance(e.get("message", {}).get("content"), str):
            return ""  # back at the user's prompt with no text reply this turn
    return ""


def redact(d: dict) -> dict:
    """Under TOTE_REDACT=1 a log entry keeps rows, grounds and call ids but no text from the transcript; the reasons
    are dropped too, since some quote what the cited output shows."""
    return {k: v for k, v in d.items() if k not in ("claim", "span", "quote", "why")} if transcript.REDACT else d


def skip(hook: dict, why: str, **extra) -> None:
    """Log an early exit, so a quiet hook can be told apart from one that never ran."""
    LOG.parent.mkdir(parents=True, exist_ok=True)
    with open(LOG, "a") as f:
        f.write(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "session": hook.get("session_id"), "skip": why,
                            "hook_keys": sorted(hook), **extra}) + "\n")


def run_ask(hook: dict, turn: str, rendered: tuple, t0: float) -> None:
    r = ask.ask_rendered(*rendered)
    rows, flags = r["rows"], r["flags"]
    LOG.parent.mkdir(parents=True, exist_ok=True)
    with open(LOG, "a") as f:
        f.write(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "session": hook.get("session_id"), "cwd": hook.get("cwd"),
                            "rows": len(rows), "flagged": len(flags),
                            "cost": round(r["cost"], 4), "seconds": round(time.time() - t0, 1),
                            "flags": [redact({"row": i, "span": rows[i - 1], "why": [w]}) for i, w in sorted(flags.items())],
                            "judged": len(r["judged"]),
                            "judged_rows": [] if transcript.REDACT else [{"row": i, "span": rows[i - 1], "why": w} for i, w in sorted(r["judged"].items())],
                            "usage": r.get("usage"), **({"redacted": True} if transcript.REDACT else {})}) + "\n")
    # nothing goes to the user: the agent re-checks flagged sentences and writes only corrections, so what the user
    # reads is a fix ("Correction: …") or nothing. A wrong flag costs a silent check, not a paragraph of dispute
    if not flags:
        return
    with open(BLOCKED, "a") as f:
        f.write(turn + "\n")
    lines = "\n".join(f"{n}. \"{rows[i - 1][:200]}\" ({w[:200]})" for n, (i, w) in enumerate(sorted(flags.items()), 1))
    reason = ("tote: re-check these sentences from your last reply before the user relies on them:\n"
              f"{lines}\n\n"
              "Check each one quickly with a tool if it can be checked. Then reply with corrections only, one line each, numbered "
              "like the list: \"Correction (n): <the corrected fact>.\" For a sentence that holds, or a guess you cannot check, write nothing "
              "about it. Do not repeat or rewrite the earlier reply, and do not mention tote or these instructions. If "
              "nothing needs correcting, reply with the single word \"Checked.\" (an empty turn makes Claude Code ask again).")
    print(json.dumps(silent(reason)))


def silent(reason: str) -> dict:
    """Feedback only the agent reads: a Stop hook's additionalContext continues the conversation without a chat
    message, where a block reason is shown to the user as "Stop hook feedback"."""
    return {"hookSpecificOutput": {"hookEventName": "Stop", "additionalContext": reason}}


def main():
    hook = json.load(sys.stdin)
    if os.environ.get("TOTE_CHECK") == "0":
        return skip(hook, "disabled")
    # block at most once per user turn. stop_hook_active is shared with every other Stop hook, so another hook's block
    # would otherwise switch this one off for the rest of the turn
    turn = f"{hook.get('session_id')}:{hook.get('prompt_id')}"
    seen = set(BLOCKED.read_text().split()) if BLOCKED.exists() else set()
    if hook.get("prompt_id") and turn in seen:  # the corrections turn after a send-back is not checked again
        return skip(hook, "corrections turn after a block")
    tp = hook.get("transcript_path")
    if not tp or not Path(tp).exists():
        return skip(hook, "no transcript", transcript_path=tp)
    reply = hook.get("last_assistant_message") or final_reply(tp)
    if len(reply) < MIN_CHARS:
        return skip(hook, "short reply", chars=len(reply), head="" if transcript.REDACT else reply[:80])
    if not sampled(turn):
        return skip(hook, "not sampled", sample=SAMPLE)
    if DAILY_USD != float("inf") and (spent := spent_today()) >= DAILY_USD:
        # said once a day, on the first skipped reply: after it, a quiet reply means unchecked, not passed
        flag = LOG.parent / f"budget-out-{time.strftime('%Y-%m-%d')}"
        if not flag.exists():
            flag.touch()
            print(json.dumps({"systemMessage": f"tote: today's ${DAILY_USD:.0f} check budget is spent; replies are unchecked until tomorrow"}))
        return skip(hook, "daily budget spent", spent=round(spent, 4), budget=DAILY_USD)
    t0 = time.time()
    for events in transcript.segments(tp):
        try:
            context, found, tools, users, summary = transcript.render(events, {"reply": reply[:80]}, window_chars=WINDOW)
            break
        except SystemExit:
            continue
    else:
        return skip(hook, "reply not found in transcript", head="" if transcript.REDACT else reply[:80])
    # a project can keep some sessions away from the checker's API, e.g. any turn whose window read production user
    # content: nothing is sent, and only the pattern's name is logged, never the matching text
    if os.environ.get("TOTE_SKIP_IF") and re.search(os.environ["TOTE_SKIP_IF"], context if isinstance(context, str) else "".join(context).replace(transcript.CHUNK, "\n\n")):
        print(json.dumps({"systemMessage": "tote: not checked; this turn's transcript matches TOTE_SKIP_IF, so it is not sent to the checker"}))
        return skip(hook, "TOTE_SKIP_IF matched")
    run_ask(hook, turn, (context, found, tools, users, summary), t0)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # a crash would otherwise pass the reply as if checked, with nothing logged or shown
        LOG.parent.mkdir(parents=True, exist_ok=True)
        with open(LOG, "a") as f:
            f.write(json.dumps({"time": time.strftime("%Y-%m-%dT%H:%M:%S%z"), "error": f"{type(e).__name__}: {e}"}) + "\n")
        print(json.dumps({"systemMessage": f"tote: the check failed ({type(e).__name__}), so this reply is unchecked"}))
