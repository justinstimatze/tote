"""Reading a Claude Code session transcript: its compaction segments, one rendered text with every citable source
labelled, the reply split into sentences, and the check that a quoted source really says what is quoted."""

import json
import os
import re


TOOL_CAP = 60000  # characters of each tool output shown to the checker; quotes are checked against the full output

def _backward(path: str, end: int | None = None, chunk: int = 1 << 20):
    """(offset, line) of a file's lines from `end` (default: the end of file) back to the start, without reading it whole."""
    with open(path, "rb") as f:
        pos = f.seek(0, 2) if end is None else end
        tail = b""
        while pos > 0:
            step = min(chunk, pos)
            pos -= step
            f.seek(pos)
            parts = (f.read(step) + tail).split(b"\n")
            tail = parts[0]  # may be cut mid-line; completed by the next chunk
            at = pos + len(parts[0]) + 1
            offs = []
            for x in parts[1:]:
                offs.append((at, x))
                at += len(x) + 1
            yield from reversed(offs)
        yield 0, tail



def segments(path: str):
    """Compaction segments of a transcript, newest first, each as a list of events.

    Reads backward from the end, so the newest segment, the one the hook needs, costs only its own size: a 3.5 GB
    transcript took 13.5 s and 0.9 GB to reach it by a forward pass. Never read a whole transcript into memory."""
    def parse(raw):
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return None

    def instructions_before(a: int):
        # CLAUDE.md and memory stay in the system prompt after compaction but are logged once (per session start
        # or resume), so the newest copy logged before the segment is carried into it
        for off, line in _backward(path, a):
            if b'"type":"instructions"' in line and (e := parse(line)) is not None:
                return e
        return None

    def instructions_after(b: int):
        with open(path, "rb") as f:
            f.seek(b)
            while line := f.readline():
                if b'"type":"instructions"' in line and (e := parse(line)) is not None:
                    return e
        return None

    seg_end, lines = None, []  # lines of the current segment, newest first
    for off, line in _backward(path):
        if seg_end is None:
            seg_end = off + len(line) + (1 if line else 0)
        if b'"compact_boundary"' in line:
            # the boundary line closes the older segment; this one starts on the next line. Messages compaction kept
            # verbatim are logged above the boundary, listed only by uuid in it: the agent still sees them, so they
            # open this segment
            lines.extend(_preserved(path, off, line))
            yield _segment(path, off + len(line) + 1, seg_end, lines, instructions_before, instructions_after)
            seg_end, lines = off + len(line) + 1, [line]
        else:
            lines.append(line)
    if seg_end is not None:
        yield _segment(path, 0, seg_end, lines, instructions_before, instructions_after)



def _preserved(path: str, off: int, boundary: bytes) -> list[bytes]:
    """Lines above a compact boundary that compaction kept verbatim, newest first."""
    try:
        meta = json.loads(boundary).get("compactMetadata") or {}
    except json.JSONDecodeError:
        return []
    kept = meta.get("preservedMessages") or {}
    want = set(kept.get("allUuids") or kept.get("uuids") or [])
    head = (meta.get("preservedSegment") or {}).get("headUuid")
    out = []
    for _, line in _backward(path, off):
        if not want:
            break
        try:
            u = json.loads(line).get("uuid") if b'"uuid"' in line else None
        except (json.JSONDecodeError, AttributeError):
            u = None
        if u in want:
            out.append(line)
            want.discard(u)
            if u == head:
                break
    return out



def _segment(path, a, b, lines, before_of, after_of):
    before = before_of(a)
    if before is None and (later := after_of(b)) is not None:
        e = json.loads(json.dumps(later))  # only a later copy exists (e.g. logged at a resume): use it, labelled as such
        for f in e["attachment"].get("files") or []:
            f["path"] = f"{f.get('path', '')} (copy logged later in the session; may differ from what this segment saw)"
        before = e
    events = [before] if before is not None else []
    for line in reversed(lines):
        if line.strip() and (e := _parse_line(line)) is not None:
            events.append(e)
    return events



def _parse_line(line: bytes):
    try:
        return json.loads(line)
    except json.JSONDecodeError:
        return None



def text_of(c) -> str:
    if isinstance(c, str):
        return c
    return "\n".join(x.get("text", "") if isinstance(x, dict) else str(x) for x in c or [])



def context_of(e) -> list[tuple[str, str]]:
    """Text the agent was given outside the chat: instruction files (CLAUDE.md, MEMORY.md), hook injections,
    hook system messages and attached files. Transcripts record these as `attachment` events."""
    a = e.get("attachment") if e.get("type") == "attachment" else None
    if not isinstance(a, dict):
        return []
    t = a.get("type")
    if t == "instructions":
        return [(f.get("path", "instructions"), f.get("content") or "") for f in a.get("files") or [] if isinstance(f, dict)]
    if t == "hook_success" and a.get("hookEvent") in ("UserPromptSubmit", "SessionStart"):
        c = a.get("content")  # stdout of a prompt or session hook, which the agent sees (e.g. the context size line)
        return [(f"{a.get('hookName', 'hook')} hook", str(c))] if isinstance(c, str) and c.strip() else []
    if t in ("hook_additional_context", "hook_system_message"):
        c = a.get("content")
        return [(f"{a.get('hookName', 'hook')} hook", "\n".join(c) if isinstance(c, list) else str(c or ""))]
    if t == "file":
        c = a.get("content")
        return [(f"attached file {a.get('filename', '')}", c if isinstance(c, str) else json.dumps(c))]
    if t == "session_context":  # what the harness attached at session start, e.g. gitStatus with the current branch
        c = a.get("context")
        if isinstance(c, dict):  # the user's email address is identity, not evidence: it stays out of the check
            return [(f"session context: {k}", str(v)) for k, v in c.items() if k != "userEmail"]
        return [("session context", str(c or ""))]
    return []



CHUNK = "\x1e"  # separates cache chunks inside the stable part; ask.py sends each as its own content block



def window(out: list[str], pinned: set[int], chars: int, step: int = 20, jump: int = 40_000) -> tuple[str, str]:
    """Instruction blocks plus the recent part of the transcript, as (stable, tail).

    The stable part is chunks of `step` blocks joined by CHUNK. Each chunk goes to the API as its own content block
    with the cache mark on the last, so a later turn re-reads the chunks it shares with an earlier one at a tenth of
    the price and writes only what is new. That only works while the window's start stays put, so the start snaps to a
    grid of block positions every `jump` characters of transcript. The grid never moves as the transcript grows, so the
    start changes once per `jump` characters, and the window shows at most about `jump` characters more than `chars`."""
    body = [i for i in range(len(out)) if i not in pinned]
    keep, total = len(body), 0
    while keep > 0 and total + len(out[body[keep - 1]]) <= chars:
        keep -= 1
        total += len(out[body[keep]])
    grid, run = [0], 0
    for k, i in enumerate(body):
        run += len(out[i])
        if run >= jump * len(grid):
            grid.append(k + 1)
    start = max(g for g in grid if g <= keep)
    cut = max(start, ((len(body) - 1) // step) * step)
    head = [out[i] for i in sorted(pinned)] + ([f"[... {start} earlier transcript blocks omitted]"] if start else [])
    # chunk edges sit on fixed multiples of `step`, so a chunk, once complete, is the same block on every later turn
    edges = [start] + list(range((start // step + 1) * step, cut, step)) + [cut]
    chunks = ["\n\n".join(head)] + ["\n\n".join(out[i] for i in body[a:b]) for a, b in zip(edges, edges[1:]) if b > a]
    return CHUNK.join(c for c in chunks if c), "\n\n".join(out[i] for i in body[cut:])



TOOL_NAMES: dict[str, str] = {}  # call id -> tool name, filled by render; ids are unique across sessions, so threads can share it



def render(events, targets: dict[str, str], window_chars: int | None = None):
    """One transcript text up to the last target reply, with each target labelled in place.

    Returns the text, each target's reply text and position, and every citable source with its position
    (tool outputs by call id, user messages, the summary), so code can reject evidence from after a reply."""
    out, tools, users, summary, found, inputs, seen, pinned = [], {}, [], ("", -1), {}, {}, set(), set()
    for e in events:
        for label, text in context_of(e):  # CLAUDE.md, memory, hook injections: what the agent was told, citable
            if text.strip() and text not in seen:
                seen.add(text)
                cid = f"ctx-{len(seen)}"
                tools[cid] = (text, len(out), "")
                cut = text if len(text) <= TOOL_CAP else text[:TOOL_CAP] + f"\n[... {len(text) - TOOL_CAP} more characters]"
                if e.get("attachment", {}).get("type") in ("instructions", "session_context"):
                    pinned.add(len(out))  # CLAUDE.md and memory stay in view however long the session
                out.append(f"[CONTEXT {cid}] {label}\n{cut}")
        a = e.get("attachment") if e.get("type") == "attachment" else None
        if isinstance(a, dict) and a.get("type") == "queued_command" and (a.get("origin") or {}).get("kind") == "human":
            users.append((a.get("prompt", ""), len(out)))  # a message the user sent while the agent was mid-turn
            out.append(f"[USER, sent mid-turn]\n{a.get('prompt', '')}")
            continue
        m = e.get("message") if isinstance(e.get("message"), dict) else None
        if not m or e.get("type") not in ("user", "assistant"):
            continue
        c = m.get("content")
        if e["type"] == "user" and isinstance(c, str):
            if summary[1] < 0 and c.startswith("This session is being continued"):
                summary = (c, len(out))
                pinned.add(len(out))
                out.append(f"[SESSION SUMMARY]\n{c}")
            elif c.startswith("<task-notification>"):
                # a background agent's result: the agent read it, so a reply relaying it is backed by it
                body = re.search(r"<result>(.*?)</result>", c, re.S)
                tid = re.search(r"<task-id>([^<]+)</task-id>", c)
                if body:
                    cid = f"task-{tid.group(1) if tid else len(out)}"
                    full = body.group(1).strip()
                    tools[cid] = (full, len(out), "")
                    cut = full if len(full) <= TOOL_CAP else full[:TOOL_CAP] + f"\n[... {len(full) - TOOL_CAP} more characters]"
                    out.append(f"[CONTEXT {cid}] background agent result\n{cut}")
            elif not c.startswith("<"):
                users.append((c, len(out)))
                out.append(f"[USER]\n{c}")
            continue
        for b in c if isinstance(c, list) else []:
            t = b.get("type")
            if t == "text" and e["type"] == "assistant":
                name = next((n for n, pre in targets.items() if b["text"].startswith(pre) and n not in found), None)
                if name:
                    found[name] = (b["text"], len(out))
                    out.append(f"[REPLY UNDER AUDIT: {name}]\n{b['text']}")
                    if len(found) == len(targets):
                        context = window(out, pinned, window_chars) if window_chars else "\n\n".join(out)
                        return context, found, tools, users, summary
                else:
                    out.append(f"[AGENT]\n{b['text']}")
            elif t == "text" and e["type"] == "user" and not b["text"].startswith("<"):
                users.append((b["text"], len(out)))
                out.append(f"[USER]\n{b['text']}")
            elif t == "tool_use":
                inputs[b["id"]] = json.dumps(b["input"])
                TOOL_NAMES[b["id"]] = b.get("name", "")
                s = inputs[b["id"]]
                # head and tail: a long input (an agent brief, a Write) often ends with its output path or constraints
                shown = s if len(s) <= 8000 else f"{s[:5000]} [... {len(s) - 7000} chars omitted ...] {s[-2000:]}"
                out.append(f"[CALL {b['id']}] {b['name']} {shown}")
            elif t == "tool_result":
                full = text_of(b.get("content"))
                tools[b["tool_use_id"]] = (full, len(out), inputs.get(b["tool_use_id"], ""))  # input kept to catch self-citation
                cut = full if len(full) <= TOOL_CAP else full[:TOOL_CAP] + f"\n[... {len(full) - TOOL_CAP} more characters]"
                out.append(f"[OUTPUT {b['tool_use_id']}]\n{cut}")
    raise SystemExit(f"no assistant reply starts with {sorted(set(targets.values()) - {v for v in targets.values() if any(f[0].startswith(v) for f in found.values())})}")



def unwrap(text: str) -> list[str]:
    """Rejoin hard-wrapped prose: a line continues the previous one unless it starts a block or follows a blank."""
    lines, starts = [], re.compile(r"^\s*([-*|#>]|\d+\.\s|```)")
    for line in text.splitlines():
        if lines and lines[-1].strip() and line.strip() and not starts.match(line) and not lines[-1].strip().startswith(("|", "#", "```")):
            lines[-1] = lines[-1].rstrip() + " " + line.strip()
        else:
            lines.append(line)
    return lines



def split_rows(reply: str) -> list[str]:
    rows, in_code = [], False
    for line in unwrap(reply):
        s = line.strip()
        if s.startswith("```"):
            in_code = not in_code
            continue
        if not s or in_code or set(s) <= set("|-: "):
            continue
        if s.startswith("|") or s.startswith("#"):
            rows.append(s)
            continue
        body = re.sub(r"^([-*]|\d+\.)\s+", "", s)
        rows.extend(x for x in re.split(r"(?<=[.!?])\s+(?=[A-Z`\"(])", body) if x.strip())
    return rows



def norm(s: str) -> str:
    return " ".join(s.split())



# TOTE_REDACT=1: no checker response is written to the disk cache, for projects whose transcripts may hold user content
REDACT = os.environ.get("TOTE_REDACT") == "1"



LINENO = re.compile(r"(?m)^\s*\d+(?:\t|→)")

FALLBACK_MIN = 40  # characters a quote needs before an output other than the cited one may back it

def call_text(raw: str) -> str:
    """A tool call's input as plain text: every string value, decoded, one after another."""
    try:
        d = json.loads(raw) if raw else {}
    except ValueError:
        return raw
    vals, stack = [], [d]
    while stack:
        x = stack.pop()
        if isinstance(x, str):
            vals.append(x)
        elif isinstance(x, dict):
            stack.extend(reversed(list(x.values())))
        elif isinstance(x, list):
            stack.extend(reversed(x))
    return "\n".join(vals)



def verify(r: dict, at: int, tools: dict, users: list, summary: tuple) -> bool:
    """The quote is an exact substring of the cited source, and the source comes before the reply at position `at`."""
    q = norm(r["quote"])
    if not q:
        return False
    def in_input(s):
        # the input is stored as JSON, where quotes, newlines and non-ASCII are escaped; match the decoded values too
        return s[1] < at and (q in norm(s[2]) or q in norm(call_text(s[2])))

    def has(s):
        # Claude Code's Read output starts each line with "N\t"; a quote that spans lines skips those prefixes
        return s[1] < at and (q in norm(s[0]) or q in norm(LINENO.sub("", s[0])))

    # the checker sometimes files a quote under the wrong half of the same call (a command it read back from the output
    # of a cat, labelled tool_call), so the cited call's input and output each back a quote from the other
    # a quote found only in a call's input is the agent's own words: it shows what the agent did, never a fact about
    # the world, so ground_of turns it into self_cite unless the claim is about the agent's own action
    if r["backing"] == "tool_call":
        src = tools.get(r["call_id"])
        if src is not None and in_input(src):
            r["in_input"] = True
            return True
        return src is not None and has(src)
    if r["backing"] in ("tool_output", "context"):
        src = tools.get(r["call_id"])
        if src is not None and has(src):
            return True
        if src is not None and in_input(src):
            r["in_input"] = True
            return True
        # the checker sometimes names the wrong call id for a quote it copied exactly. Only a long quote may be rescued
        # by another output, since a short one ("passed", "main") turns up somewhere in any long session; the rescuing
        # call is recorded on the claim so the log shows how often this fires
        if len(q) < FALLBACK_MIN:
            return False
        hit = next((cid for cid, s in tools.items() if has(s)), None)
        if hit:
            r["fallback_call"] = hit
        return hit is not None
    if r["backing"] == "user_message":
        # a user message from before the last compaction survives only as a quote in the summary
        return any(pos < at and q in norm(u) for u, pos in users) or (summary[1] < at and q in norm(summary[0]))
    if r["backing"] == "summary":
        return (summary[1] < at and q in norm(summary[0])) or any(pos < at and q in norm(u) for u, pos in users)
    return False



NUMS = re.compile(r"(?<![\w.])\d[\d,]*(?:\.\d+)?%?")



def nums(s: str) -> set:
    return {n.rstrip("%").replace(",", "") for n in NUMS.findall(s)}



IDS = [re.compile(r"`([^`\n]{2,80})`"),  # backticked code, paths, commands
       # a path: rooted (~/, ./, /) or with two slashes or a file extension; "pause/play" in prose is not one
       re.compile(r"(?<![\w/])(?:[~.]?/[\w.-]+(?:/[\w.-]+)*|[\w.-]+/[\w.-]+/[\w./-]+|[\w.-]+/[\w/-]*\.\w{1,5}\b)"),
       re.compile(r"\b[\w-]+\.(?:py|go|ts|tsx|js|md|json|jsonl|toml|yaml|yml|sh|txt|csv|html|sql|rs)\b"),  # a file name
       re.compile(r"\b[a-z][a-z0-9]*_[a-z0-9_]+\b"),  # snake_case
       re.compile(r"\b[a-z]+[A-Z][A-Za-z0-9]*\b")]  # camelCase



def identifiers(span: str) -> tuple[set, set]:
    """The checkable names and numbers in a claim's span. Single digits are left out: they are mostly list numbering."""
    names = {m.group(m.lastindex or 0).strip() for p in IDS for m in p.finditer(span)}
    return {n for n in names if len(n) > 2}, {n for n in nums(span) if not (len(n) == 1)}
