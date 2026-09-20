"""Checks read the outcome and never the transcript.

Every check is a small function keyed by name in sentences.json, taking the
parameters written beside it there. A check answers a dict with passed and a
detail a reader can act on.
"""

import json
import ssl
import time
import urllib.error
import urllib.request
import uuid

from . import mcp

USER_AGENT = "kitbash-bench/1"
HTTP_TIMEOUT = 20


class Context:
    """What a check is allowed to know: the member's session and the host."""

    def __init__(self, alias, config, member, domain, pre_ids=None, since=None, variables=None):
        self.alias = alias
        self.config = config
        self.member = member
        self.domain = domain
        self.pre_ids = set(pre_ids or [])
        self.since = since
        self.variables = dict(variables or {})

    def call(self, tool, arguments=None):
        return mcp.call_once(self.alias, tool, arguments, config=self.config)

    def processes(self):
        answer = self.call("proc_list", {})
        return answer.get("processes") or []

    def url_of(self, process):
        url = process.get("url")
        if url:
            return url
        if self.domain:
            return "https://%s.%s.%s" % (process.get("name"), self.member, self.domain)
        return None


def fetch(url, method="GET", body=None, headers=None):
    """One request. A status is an answer, not an exception."""
    data = None
    sent = {"User-Agent": USER_AGENT}
    sent.update(headers or {})
    if body is not None:
        data = json.dumps(body).encode()
        sent["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=data, method=method, headers=sent)
    context = ssl.create_default_context()
    try:
        with urllib.request.urlopen(request, timeout=HTTP_TIMEOUT, context=context) as answer:
            return answer.status, answer.read(65536).decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(65536).decode("utf-8", "replace")
    except Exception as exc:
        return None, "%s: %s" % (type(exc).__name__, exc)


def _candidates(ctx, params):
    """The Processes a check is about: one named, or the ones this run made."""
    name = params.get("process")
    processes = ctx.processes()
    if name:
        found = [p for p in processes if p.get("name") == name]
        # A named Process that is gone is still an address the user holds, so
        # the check asks that address rather than giving up on the listing.
        return found or [{"name": name, "state": "gone"}]
    scope = params.get("scope", "new")
    chosen = []
    for process in processes:
        if scope == "new" and process.get("id") in ctx.pre_ids:
            continue
        if ctx.url_of(process):
            chosen.append(process)
    return chosen


def _statuses(params):
    status = params.get("status", 200)
    return [status] if isinstance(status, int) else list(status)


def http_ok(ctx, params):
    """The address answers. Which address is the one this run produced."""
    candidates = _candidates(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    wanted = _statuses(params)
    contains = params.get("body_contains") or []
    least = params.get("min_bytes", 0)
    tried = []
    for process in candidates:
        base = ctx.url_of(process)
        url = base.rstrip("/") + params.get("path", "/")
        status, body = fetch(url)
        note = {"url": url, "status": status, "bytes": len(body or ""), "state": process.get("state")}
        missing = [m for m in contains if m.lower() not in (body or "").lower()]
        if status in wanted and len(body or "") >= least and not missing:
            return {"passed": True, "detail": "%s answered %s" % (url, status), "seen": note}
        if missing:
            note["missing"] = missing
        note["body"] = (body or "")[:200]
        tried.append(note)
    return {"passed": False, "detail": "no address answered as asked", "tried": tried}


def http_post_roundtrip(ctx, params):
    """An item posted comes back. The check writes and reads its own marker."""
    candidates = _candidates(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    marker = "bench-%s" % uuid.uuid4().hex[:12]
    payload = dict(params.get("payload") or {"text": "{marker}", "author": "bench"})
    payload = {k: (marker if v == "{marker}" else v) for k, v in payload.items()}
    tried = []
    for process in candidates:
        base = ctx.url_of(process).rstrip("/")
        post_status, post_body = fetch(base + params.get("post_path", "/"), method="POST", body=payload)
        get_status, get_body = fetch(base + params.get("get_path", "/"))
        note = {
            "url": base,
            "post_status": post_status,
            "get_status": get_status,
            "marker": marker,
            "post_body": (post_body or "")[:200],
        }
        if post_status in (200, 201, 204) and marker in (get_body or ""):
            return {"passed": True, "detail": "%s took the item and gave it back" % base, "seen": note}
        tried.append(note)
    return {"passed": False, "detail": "no address took an item and gave it back", "tried": tried}


def _items(body):
    """The items of a board, whichever of the two shapes it answers with."""
    try:
        decoded = json.loads(body or "")
    except Exception:
        return []
    if isinstance(decoded, dict):
        decoded = decoded.get("items")
    return [item for item in decoded or [] if isinstance(item, dict)]


def http_item_posted(ctx, params):
    """An item the run put on the board is there, read off the board itself.

    The board is the fixture this round deployed, never a member's own
    Process: what a sentence asks an agent to post goes into the round and is
    read back out of it here.
    """
    candidates = _candidates(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    author = params.get("author")
    contains = params.get("text_contains") or []
    tried = []
    for process in candidates:
        url = ctx.url_of(process).rstrip("/") + params.get("path", "/api/items")
        status, body = fetch(url)
        items = _items(body)
        for item in items:
            text = str(item.get("text") or "").strip()
            if author and item.get("author") != author:
                continue
            if not text or [m for m in contains if m.lower() not in text.lower()]:
                continue
            return {
                "passed": True,
                "detail": "%s carries an item by %s: %s" % (url, item.get("author"), text[:120]),
                "seen": item,
            }
        tried.append({"url": url, "status": status, "items": len(items), "body": (body or "")[:200]})
    return {
        "passed": False,
        "detail": "no item by %s is on the board" % (author or "this run"),
        "tried": tried,
    }


def proc_running(ctx, params):
    """proc_list says the Process is up."""
    wanted = params.get("state", "running")
    for process in _candidates(ctx, params):
        if process.get("state") == wanted:
            return {"passed": True, "detail": "%s is %s" % (process.get("name"), wanted)}
    return {
        "passed": False,
        "detail": "no Process is %s" % wanted,
        "tried": [{"name": p.get("name"), "state": p.get("state")} for p in _candidates(ctx, params)],
    }


def proc_scheduled(ctx, params):
    """A job exists: a Process registered with a cron expression, waiting.

    proc_list answers a line per Process, and since issue #154 that line
    carries the cron expression and the next tick. A host that predates it
    answers the state alone, so a Process that says scheduled and nothing else
    is read again by its Package, which is the call the fix removes.
    """
    name = params.get("process")
    found = []
    for process in ctx.processes():
        if name and process.get("name") != name:
            continue
        if not name and params.get("scope", "new") == "new" and process.get("id") in ctx.pre_ids:
            continue
        schedule = process.get("schedule")
        full = process
        if not schedule and process.get("state") == "scheduled" and process.get("package"):
            for detailed in ctx.call("proc_list", {"package": process["package"]}).get("processes") or []:
                if detailed.get("id") == process.get("id"):
                    full = detailed
                    schedule = detailed.get("schedule")
        if schedule or full.get("state") == "scheduled":
            found.append(
                {
                    "name": full.get("name"),
                    "schedule": schedule,
                    "state": full.get("state"),
                    "nextRun": full.get("nextRun"),
                }
            )
    if found:
        return {"passed": True, "detail": "a job is registered: %s" % json.dumps(found), "seen": found}
    return {"passed": False, "detail": "no Process of this run carries a schedule"}


def tel_schedule(ctx, params):
    """A kitbash.schedule record exists, which is how a tick is read."""
    arguments = {"signal": "metrics", "limit": params.get("limit", 20)}
    if ctx.since and params.get("since_round", True):
        arguments["since"] = ctx.since
    try:
        answer = ctx.call("tel_query", arguments)
    except mcp.McpError as exc:
        return {"passed": False, "detail": "tel_query refused: %s" % exc.detail[:200]}
    records = [r for r in answer.get("records") or [] if "schedule" in json.dumps(r)]
    if records:
        return {"passed": True, "detail": "%d kitbash.schedule records" % len(records), "seen": records[:3]}
    return {"passed": False, "detail": "no kitbash.schedule record since the round started"}


def all_of(ctx, params):
    """Every check passes, and the first that does not is the detail."""
    details = []
    for spec in params.get("checks") or []:
        result = run(ctx, spec)
        details.append({"name": spec.get("name"), **result})
        if not result.get("passed"):
            return {"passed": False, "detail": "%s: %s" % (spec.get("name"), result.get("detail")), "parts": details}
    return {"passed": True, "detail": "; ".join(d.get("detail", "") for d in details), "parts": details}


REGISTRY = {
    "http_ok": http_ok,
    "http_post_roundtrip": http_post_roundtrip,
    "http_item_posted": http_item_posted,
    "proc_running": proc_running,
    "proc_scheduled": proc_scheduled,
    "tel_schedule": tel_schedule,
    "all_of": all_of,
}


def run(ctx, spec, attempts=1, delay=5):
    """Run one check by name. Several attempts because a start takes a moment."""
    function = REGISTRY.get(spec.get("name"))
    if not function:
        return {"passed": False, "detail": "no check named %s" % spec.get("name")}
    result = {"passed": False, "detail": "not run"}
    for attempt in range(max(1, attempts)):
        if attempt:
            time.sleep(delay)
        try:
            result = function(ctx, spec.get("params") or {})
        except mcp.McpError as exc:
            result = {"passed": False, "detail": "the surface refused: %s" % exc.detail[:200]}
        except Exception as exc:
            result = {"passed": False, "detail": "%s: %s" % (type(exc).__name__, exc)}
        if result.get("passed"):
            break
    result["check"] = spec.get("name")
    return result
