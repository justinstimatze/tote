"""The code that decides what tote flags, clears and counts. No network: every test builds its own inputs."""

import json

from tote import ask, misses, outcomes, transcript


def read(path, out, pos):
    return (out, pos, json.dumps({"file_path": path}))


def edit(path, pos):
    return ("ok", pos, json.dumps({"file_path": path, "old_string": "a", "new_string": "b"}))


# transcript.verify: a quote must really be in the cited source, before the reply

def test_verify_exact_quote():
    tools = {"c1": ("total 42 rows", 1, "")}
    assert transcript.verify({"quote": "42 rows", "call_id": "c1", "backing": "tool_output"}, 5, tools, [], ("", -1))


def test_verify_rejects_quote_from_after_the_reply():
    tools = {"c1": ("total 42 rows", 9, "")}
    assert not transcript.verify({"quote": "42 rows", "call_id": "c1", "backing": "tool_output"}, 5, tools, [], ("", -1))


def test_verify_rejects_made_up_quote():
    tools = {"c1": ("total 42 rows", 1, "")}
    assert not transcript.verify({"quote": "43 rows", "call_id": "c1", "backing": "tool_output"}, 5, tools, [], ("", -1))


def test_verify_reads_json_escaped_input():
    tools = {"c1": ("", 1, json.dumps({"command": 'echo "hi there"'}))}
    assert transcript.verify({"quote": 'echo "hi there"', "call_id": "c1", "backing": "tool_call"}, 5, tools, [], ("", -1))


def test_verify_other_output_rescues_only_a_long_quote():
    long = "x" * transcript.FALLBACK_MIN
    tools = {"c1": ("unrelated", 1, ""), "c2": (f"before {long} after", 2, "")}
    assert transcript.verify({"quote": long, "call_id": "c1", "backing": "tool_output"}, 5, tools, [], ("", -1))
    assert not transcript.verify({"quote": "before", "call_id": "c1", "backing": "tool_output"}, 5, tools, [], ("", -1))


# ask.edited_after: a quote from a read goes stale when the file is edited before the reply

def test_read_then_edit_is_stale():
    tools = {"r": read("/a.py", "x = 1", 1), "e": edit("/a.py", 3)}
    assert ask.edited_after("r", 5, tools) == "/a.py"


def test_edit_cited_for_itself_is_not_stale():
    tools = {"e1": edit("/a.py", 2), "e2": edit("/a.py", 3)}
    assert ask.edited_after("e1", 5, tools) == ""


def test_edit_after_the_reply_does_not_count():
    tools = {"r": read("/a.py", "x = 1", 1), "e": edit("/a.py", 7)}
    assert ask.edited_after("r", 5, tools) == ""


def test_edit_to_another_file_does_not_count():
    tools = {"r": read("/a.py", "x = 1", 1), "e": edit("/b.py", 3)}
    assert ask.edited_after("r", 5, tools) == ""


# transcript.render: a background agent's result is citable

def test_render_keeps_task_notification_as_citable_context():
    events = [
        {"type": "user", "message": {"content": "<task-notification>\n<task-id>abc</task-id>\n<result>Answering as: x\nThe verdict is yes.</result>\n</task-notification>"}},
        {"type": "assistant", "message": {"content": [{"type": "text", "text": "My reply: the verdict is yes."}]}},
    ]
    context, found, tools, users, summary = transcript.render(events, {"reply": "My reply"})
    assert "task-abc" in tools and "The verdict is yes." in tools["task-abc"][0]
    assert transcript.verify({"quote": "The verdict is yes.", "call_id": "task-abc", "backing": "context"},
                         found["reply"][1], tools, users, summary)


def test_render_still_drops_other_system_text():
    events = [
        {"type": "user", "message": {"content": "<system-reminder>secret instructions</system-reminder>"}},
        {"type": "assistant", "message": {"content": [{"type": "text", "text": "My reply."}]}},
    ]
    context, *_ = transcript.render(events, {"reply": "My reply"})
    assert "secret instructions" not in context


# outcomes: block wording, numbered flags, corrections

NEW_BLOCK = ("Stop hook feedback:\ntote: re-check these sentences from your last reply before the user relies on them:\n"
             '1. "Every status file is fresh." (not sure: x)\n2. "It costs $5." (not sure: y)\n')
OLD_BLOCK = 'Stop hook feedback:\ntote: 2 sentences need a source:\n- row 3, "Alpha is 3": unbacked\n- "Beta is 4" (no quote)\n'


def test_block_and_flag_patterns_match_old_and_new_wording():
    for text, want in ((NEW_BLOCK, ["Every status file is fresh.", "It costs $5."]), (OLD_BLOCK, ["Alpha is 3", "Beta is 4"])):
        assert outcomes.BLOCK.match(text)
        assert outcomes.FLAG.findall(text) == want


def test_numbered_correction_maps_to_its_flag():
    rows = outcomes.score(NEW_BLOCK, 2, "Correction (2): It costs $7.")
    assert [r["outcome"] for r in rows] == ["checked", "corrected"]
    assert rows[1]["type"] == "value"


def test_checked_with_no_tool_call_is_ignored():
    rows = outcomes.score(NEW_BLOCK, 0, "Checked.")
    assert [r["outcome"] for r in rows] == ["ignored", "ignored"]


def test_correction_types():
    assert outcomes.correction_type("tote counts Correction turns", "tote doesn't count them yet") == "existence"
    assert outcomes.correction_type("6 of 8 were sent back", "5 of 8 were sent back") == "value"
    assert outcomes.correction_type("every status file is fresh", "the two status files I checked are fresh") == "scope"


def test_turn_cost_counts_each_message_once_and_prices_1h_writes():
    u = {"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 1_000_000,
         "cache_creation": {"ephemeral_1h_input_tokens": 1_000_000, "ephemeral_5m_input_tokens": 0}}
    assert outcomes.turn_cost({"m1": ("claude-opus-5-5", u)}) == 8.0  # 4.00/M input, 2x for a 1-hour write


# misses: what counts as the agent taking something back

def test_retraction_pattern():
    assert misses.RETRACT.search("I was wrong earlier: the cap exists.")
    assert misses.RETRACT.search("I asserted the default without checking it.")
    assert not misses.RETRACT.search("Once green I'll merge without checking back with you.")
    assert not misses.RETRACT.search("The gate is wrong, not the walkers.")


def test_absence_pattern():
    assert ask.ABSENCE.search("Part 3 never shows that door.")
    assert ask.ABSENCE.search("`script/scenes.json` doesn't exist.")
    assert ask.ABSENCE.search("There are no other callers.")
    assert not ask.ABSENCE.search("The door appears in part 3.")


def test_segment_keeps_messages_compaction_preserved(tmp_path):
    lines = [
        {"type": "user", "uuid": "u-old", "message": {"content": "dropped by compaction"}},
        {"type": "user", "uuid": "u-kept", "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": "alpha 28"}]}},
        {"type": "system", "subtype": "compact_boundary", "uuid": "b",
         "compactMetadata": {"preservedSegment": {"headUuid": "u-kept"}, "preservedMessages": {"uuids": ["u-kept"]}}},
        {"type": "assistant", "uuid": "a1", "message": {"content": [{"type": "text", "text": "Alpha has 28 sessions."}]}},
    ]
    p = tmp_path / "t.jsonl"
    p.write_text("\n".join(json.dumps(x) for x in lines) + "\n")
    text = json.dumps(next(transcript.segments(str(p))))
    assert "alpha 28" in text and "dropped by compaction" not in text


def test_outcomes_reads_silent_send_back(tmp_path):
    ctx = 'tote: re-check these sentences from your last reply before the user relies on them:\n1. "It costs $5." (not sure: x)\n'
    lines = [{"type": "system", "subtype": "stop_hook_summary", "hookAdditionalContext": [ctx]},
             {"type": "assistant", "message": {"id": "m", "content": [{"type": "text", "text": "Correction (1): It costs $7."}]}}]
    p = tmp_path / "t.jsonl"
    p.write_text("\n".join(json.dumps(x) for x in lines) + "\n")
    (text, calls, reply, cost), = outcomes.blocks(str(p))
    assert [r["outcome"] for r in outcomes.score(text, calls, reply)] == ["corrected"]


def test_ask_load_reads_a_transcript_file(tmp_path):
    p = tmp_path / "t.jsonl"
    p.write_text(json.dumps({"type": "assistant", "uuid": "a", "message": {"content": [{"type": "text", "text": "My reply here."}]}}) + "\n")
    (context, found, *_), seg = ask.load(str(p), "My reply")
    assert found["reply"][0] == "My reply here." and seg == 0
