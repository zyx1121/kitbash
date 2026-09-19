"""Reading a transcript into the numbers a row carries.

A row is what the bench keeps: how much the sentence cost, what it called, and
whether the check passed. The transcript is read only for the counting; whether
the sentence worked is the check's answer alone.
"""

import collections
import json
import re

KITBASH_PREFIX = "mcp__kitbash__"

# What a sentence ending in a question mark is, and what a request for the user
# looks like when it does not end in one.
REQUEST_PATTERNS = [
    r"\blet me know\b",
    r"\bwould you like\b",
    r"\bdo you want\b",
    r"\bshould i\b",
    r"\bwhich (one|option|would)\b",
    r"\bplease (tell|confirm|provide|give|set|share)\b",
    r"\bi need you to\b",
    r"\btell me (which|what|whether)\b",
]


def questions_asked(text):
    """Sentences ending in a question mark, which is the count 4.5 reads."""
    if not text:
        return 0
    return len(re.findall(r"[^.!?\n]+\?", text))


def asks_user(text):
    """Whether the answer ends by handing the decision back."""
    if not text:
        return False
    lines = [line.strip() for line in text.strip().splitlines() if line.strip()]
    if lines and lines[-1].endswith("?"):
        return True
    lowered = text.lower()
    return any(re.search(pattern, lowered) for pattern in REQUEST_PATTERNS)


def _blank_transcript(agent):
    return {
        "agent": agent,
        "model": None,
        "turns": None,
        "tool_calls": 0,
        "tools": {},
        "kitbash_calls": {},
        "tool_errors": 0,
        "cost_usd": None,
        "usage": None,
        "result_text": "",
        "stop_reason": None,
        "rate_limited": False,
    }


def _lines(path):
    with open(path, encoding="utf-8", errors="replace") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            try:
                yield json.loads(line)
            except ValueError:
                continue


def parse_claude_stream(path):
    """Claude Code's stream-json: one JSON object per line, result last."""
    transcript = _blank_transcript("claude")
    tools = collections.Counter()
    kitbash = collections.Counter()
    for message in _lines(path):
        kind = message.get("type")
        if kind == "system" and message.get("subtype") == "init":
            transcript["model"] = message.get("model")
        elif kind == "assistant":
            for block in (message.get("message") or {}).get("content") or []:
                if block.get("type") == "tool_use":
                    name = block.get("name", "")
                    tools[name] += 1
                    if name.startswith(KITBASH_PREFIX):
                        kitbash[name[len(KITBASH_PREFIX):]] += 1
        elif kind == "user":
            for block in (message.get("message") or {}).get("content") or []:
                if isinstance(block, dict) and block.get("type") == "tool_result" and block.get("is_error"):
                    transcript["tool_errors"] += 1
        elif kind == "rate_limit_event":
            info = message.get("rate_limit_info") or {}
            if info.get("status") not in (None, "allowed"):
                transcript["rate_limited"] = True
        elif kind == "result":
            transcript["result_text"] = message.get("result") or ""
            transcript["usage"] = message.get("usage")
            transcript["cost_usd"] = message.get("total_cost_usd")
            transcript["turns"] = message.get("num_turns")
            transcript["stop_reason"] = message.get("subtype") or message.get("stop_reason")
            if message.get("api_error_status") in (429, "429"):
                transcript["rate_limited"] = True
            models = list((message.get("modelUsage") or {}).keys())
            if models and not transcript["model"]:
                transcript["model"] = models[0]
    transcript["tools"] = dict(tools)
    transcript["kitbash_calls"] = dict(kitbash)
    transcript["tool_calls"] = sum(tools.values())
    return transcript


CODEX_TOOL_ITEMS = ("mcp_tool_call", "command_execution", "file_change", "web_search", "patch_apply")


def parse_codex_stream(path):
    """codex exec --json: item.completed events, one turn per exec.

    What cannot be read here: a price, which Codex does not report, and a turn
    count of the shape Claude reports. turns is counted as the model's own
    messages plus its tool calls, and cost_usd stays null with the token usage
    beside it.
    """
    transcript = _blank_transcript("codex")
    tools = collections.Counter()
    kitbash = collections.Counter()
    messages = 0
    for event in _lines(path):
        kind = event.get("type")
        if kind == "item.completed":
            item = event.get("item") or {}
            item_type = item.get("type")
            if item_type == "agent_message":
                messages += 1
                transcript["result_text"] = item.get("text") or transcript["result_text"]
            elif item_type in CODEX_TOOL_ITEMS:
                if item_type == "mcp_tool_call":
                    name = "%s__%s" % (item.get("server"), item.get("tool"))
                    tools[name] += 1
                    if item.get("server") == "kitbash":
                        kitbash[item.get("tool")] += 1
                    if item.get("error") or item.get("status") not in (None, "completed"):
                        transcript["tool_errors"] += 1
                else:
                    tools[item_type] += 1
                    if item.get("exit_code") not in (None, 0) or item.get("status") == "failed":
                        transcript["tool_errors"] += 1
        elif kind == "turn.completed":
            transcript["usage"] = event.get("usage")
            transcript["stop_reason"] = "completed"
        elif kind == "turn.failed":
            transcript["stop_reason"] = "failed"
            error = json.dumps(event.get("error") or {})
            transcript["error"] = error[:400]
            if "rate limit" in error.lower() or "429" in error:
                transcript["rate_limited"] = True
        elif kind == "error":
            transcript["stop_reason"] = "error"
            transcript["error"] = json.dumps(event)[:400]
    transcript["tools"] = dict(tools)
    transcript["kitbash_calls"] = dict(kitbash)
    transcript["tool_calls"] = sum(tools.values())
    transcript["turns"] = messages + transcript["tool_calls"]
    return transcript


PARSERS = {"claude": parse_claude_stream, "codex": parse_codex_stream}


def build_row(sentence, transcript, outcome, meta):
    """One run, one row. Order is the order a reader wants it in."""
    text = transcript.get("result_text") or ""
    row = {
        "id": sentence["id"],
        "class": sentence["class"],
        "run": meta.get("run"),
        "agent": transcript.get("agent"),
        "model": transcript.get("model"),
        "kitbash_version": meta.get("kitbash_version"),
        "member": meta.get("member"),
        "prompt": meta.get("prompt", sentence.get("prompt")),
        "turns": transcript.get("turns"),
        "tool_calls": transcript.get("tool_calls"),
        "kitbash_calls": sum((transcript.get("kitbash_calls") or {}).values()),
        "kitbash_calls_by_tool": transcript.get("kitbash_calls") or {},
        "tool_errors": transcript.get("tool_errors"),
        "questions_asked": questions_asked(text),
        "asks_user": asks_user(text),
        "cost_usd": transcript.get("cost_usd"),
        "wall_ms": meta.get("wall_ms"),
        "passed": bool(outcome.get("passed")),
        "check": outcome,
        "time_to_mitigate_ms": meta.get("time_to_mitigate_ms"),
        "injected_at": meta.get("injected_at"),
        "inject_verified": meta.get("inject_verified"),
        "started_at": meta.get("started_at"),
        "ended_at": meta.get("ended_at"),
        "stop_reason": transcript.get("stop_reason"),
        "rate_limited": transcript.get("rate_limited", False),
        "timed_out": meta.get("timed_out", False),
        "exit_code": meta.get("exit_code"),
        "usage": transcript.get("usage"),
        "tools": transcript.get("tools") or {},
        "result_text": text[:2000],
        "setup": meta.get("setup"),
        "error": transcript.get("error") or meta.get("error"),
    }
    return row
