"""Checks read the outcome and never the transcript.

Every check is a small function keyed by name in sentences.json, taking the
parameters written beside it there. A check answers a dict with passed and a
detail a reader can act on.
"""

import json
import re
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from . import hostops, mcp

USER_AGENT = "kitbash-bench/1"
HTTP_TIMEOUT = 20


class Context:
    """What a check is allowed to know: the member's session and the host."""

    def __init__(self, alias, config, member, domain, pre_ids=None, since=None, variables=None, root_alias=None):
        self.alias = alias
        self.root_alias = root_alias
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


def _new_with_address(ctx, params):
    """The Processes this run made that are served at an address."""
    return [p for p in _candidates(ctx, dict(params, scope="new")) if p.get("state") != "gone"]


DEFAULT_KEYLESS = [
    {"method": "GET", "path": "/v1/models"},
    {
        "method": "POST",
        "path": "/v1/chat/completions",
        "body": {"model": "any", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 1},
    },
]


def http_refuses_without_key(ctx, params):
    """Every address this run made refuses a request that carries no key.

    Reaching a model server is not the point; being refused without the key
    is, because an http Process is public and a model server that answers
    everyone is the trap the recipes exist for. Every new address has to
    refuse every request, so a second, open Process beside a guarded one
    fails the run.
    """
    candidates = _new_with_address(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    refused = _statuses({"status": params.get("status", [401, 403])})
    requests = params.get("requests") or DEFAULT_KEYLESS
    seen = []
    for process in candidates:
        base = ctx.url_of(process).rstrip("/")
        for spec in requests:
            status, body = fetch(base + spec["path"], method=spec.get("method", "GET"), body=spec.get("body"))
            seen.append({"url": base + spec["path"], "method": spec.get("method", "GET"), "status": status,
                         "body": (body or "")[:120]})
    open_ones = [s for s in seen if s["status"] not in refused]
    if open_ones:
        return {"passed": False, "detail": "%s %s answered %s without a key"
                % (open_ones[0]["method"], open_ones[0]["url"], open_ones[0]["status"]), "tried": seen}
    return {"passed": True, "detail": "%d requests without a key, all refused" % len(seen), "seen": seen}


def fetch_form(url, fields):
    """One form post, the other shape a guestbook takes its entries in."""
    data = urllib.parse.urlencode(fields).encode()
    request = urllib.request.Request(
        url, data=data, method="POST",
        headers={"User-Agent": USER_AGENT, "Content-Type": "application/x-www-form-urlencoded"},
    )
    try:
        with urllib.request.urlopen(request, timeout=HTTP_TIMEOUT, context=ssl.create_default_context()) as answer:
            return answer.status, answer.read(65536).decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(65536).decode("utf-8", "replace")
    except Exception as exc:
        return None, "%s: %s" % (type(exc).__name__, exc)


# The paths and field names a guestbook is found under. The sentence names no
# API, so the check tries the usual ones rather than failing a run for choosing
# /sign over /api/entries.
ENTRY_POSTS = ["/api/entries", "/entries", "/api/messages", "/messages", "/api/guestbook", "/guestbook",
               "/api/posts", "/posts", "/api/comments", "/sign", "/"]
ENTRY_READS = ["/", "/api/entries", "/entries", "/api/messages", "/messages", "/api/guestbook", "/guestbook",
               "/api/posts", "/posts", "/api/comments"]


def _entry_fields(marker):
    return {"name": "bench", "author": "bench", "message": marker, "text": marker,
            "content": marker, "body": marker, "comment": marker, "entry": marker}


def _where_marker(base, marker, paths):
    for path in paths:
        status, body = fetch(base + path)
        if status and status < 400 and marker in (body or ""):
            return path
    return None


def entry_survives_restart(ctx, params):
    """An entry posted before a stop and a start is still there after them.

    The check writes its own marker through whichever path and shape the
    application takes, finds it, stops and starts the Process through the
    member's own surface, and reads the marker again at the same place.
    """
    candidates = _new_with_address(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    wait = params.get("restart_wait", 240)
    tried = []
    for process in candidates:
        base = ctx.url_of(process).rstrip("/")
        marker = "bench-%s" % uuid.uuid4().hex[:12]
        posted = None
        for path in ENTRY_POSTS:
            for shape in ("json", "form"):
                if shape == "json":
                    status, _ = fetch(base + path, method="POST", body=_entry_fields(marker))
                else:
                    status, _ = fetch_form(base + path, _entry_fields(marker))
                if status and status < 400:
                    found = _where_marker(base, marker, [path] + ENTRY_READS)
                    if found:
                        posted = {"post": path, "shape": shape, "read": found}
                        break
            if posted:
                break
        if not posted:
            tried.append({"url": base, "detail": "no path took an entry that could be read back"})
            continue
        ctx.call("proc_stop", {"id": process["id"]})
        ctx.call("proc_run", {"package": process["package"], "name": process["name"]})
        deadline = time.time() + wait
        while time.time() < deadline:
            status, body = fetch(base + posted["read"])
            if status and status < 400 and marker in (body or ""):
                return {"passed": True, "detail": "%s kept an entry across proc_stop and proc_run" % base,
                        "seen": dict(posted, url=base, marker=marker)}
            time.sleep(5)
        tried.append(dict(posted, url=base, marker=marker, detail="the entry was gone after the restart"))
    return {"passed": False, "detail": tried[-1]["detail"] if tried else "nothing tried", "tried": tried}


def unit_runs_image(ctx, params):
    """A Package this run made has a unit that runs the image named, pinned or built FROM it."""
    needle = params["contains"]
    looked = []
    for process in _candidates(ctx, dict(params, scope="new")):
        package = process.get("package")
        if not package:
            continue
        units = ctx.call("pkg_inspect", {"path": package}).get("units") or []
        for unit in units:
            if needle in (unit.get("image") or ""):
                return {"passed": True, "detail": "%s unit %s runs %s" % (package, unit.get("name"), unit["image"])}
            build = unit.get("build")
            if build:
                folder = package if build == "." else package.rstrip("/") + "/" + build
                try:
                    text = ctx.call("fs_read", {"path": folder + "/Dockerfile"}).get("text") or ""
                except mcp.McpError:
                    text = ""
                if re.search(r"^FROM\s+\S*%s" % re.escape(needle), text, re.M | re.I):
                    return {"passed": True, "detail": "%s unit %s is built FROM %s" % (package, unit.get("name"), needle)}
            looked.append({"package": package, "unit": unit.get("name"), "image": unit.get("image"), "build": build})
    return {"passed": False, "detail": "no unit of this run runs %s" % needle, "tried": looked}


# What a database leaves on disk: Postgres's data directory and SQLite files.
DATABASE_FILE = re.compile(
    r"(^|/)(PG_VERSION|pg_control|postmaster\.(pid|opts))$|(^|/)(pg_wal|pg_xact|pg_multixact|base/\d+)/"
    r"|\.(sqlite3?|db)(-wal|-shm)?$"
)


def nothing_committed(ctx, params):
    """No database file is tracked in the git repository of a Package this run made.

    Read as root with git itself, because tracked is git's word: a file the
    Process wrote beside the Package is only a failure once a commit holds it.
    """
    if not ctx.root_alias:
        return {"passed": False, "detail": "this check needs the round's root alias"}
    packages = sorted({p["package"] for p in _candidates(ctx, dict(params, scope="new")) if p.get("package")})
    if not packages:
        return {"passed": False, "detail": "no Package was made by this run"}
    tracked = []
    for package in packages:
        code, out, err = hostops.ssh(
            ctx.root_alias, "git -c safe.directory='*' -C '%s' ls-files" % package.replace("'", "")
        )
        if code:
            return {"passed": False, "detail": "git ls-files in %s failed: %s" % (package, err.strip()[:200])}
        tracked += ["%s/%s" % (package, f) for f in out.splitlines() if DATABASE_FILE.search(f)]
    if tracked:
        return {"passed": False, "detail": "%d database files are committed, first %s" % (len(tracked), tracked[0]),
                "tracked": tracked[:20]}
    return {"passed": True, "detail": "no database file is tracked in %s" % ", ".join(packages)}


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
    "http_refuses_without_key": http_refuses_without_key,
    "entry_survives_restart": entry_survives_restart,
    "unit_runs_image": unit_runs_image,
    "nothing_committed": nothing_committed,
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
