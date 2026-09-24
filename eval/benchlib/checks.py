"""Checks read the outcome and never the transcript.

Every check is a small function keyed by name in sentences.json, taking the
parameters written beside it there. A check answers a dict with passed and a
detail a reader can act on.
"""

import json
import re
import shlex
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

    def __init__(self, alias, config, member, domain, pre_ids=None, since=None, variables=None, root_alias=None,
                 uid=None):
        self.alias = alias
        self.root_alias = root_alias
        # The member's uid, which a command run as the member through the root
        # alias needs for the runtime directory of their podman.
        self.uid = uid
        self.config = config
        self.member = member
        self.domain = domain
        self.pre_ids = set(pre_ids or [])
        self.since = since
        self.variables = dict(variables or {})

    def call(self, tool, arguments=None, timeout=120):
        return mcp.call_once(self.alias, tool, arguments, config=self.config, timeout=timeout)

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


# The paths a model server answers on, by engine. An address is probed on all
# of them, because which engine it runs is the agent's choice: a proxy that
# guards /v1 in front of Ollama and passes /api through is as open as no proxy.
ENGINE_PROBES = {
    "openai": [
        {"method": "GET", "path": "/v1/models"},
        {
            "method": "POST",
            "path": "/v1/chat/completions",
            "body": {"model": "any", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 1},
        },
    ],
    "ollama": [
        {"method": "GET", "path": "/api/tags"},
        {"method": "POST", "path": "/api/generate", "body": {"model": "any", "prompt": "hi", "stream": False}},
    ],
    "llama-server": [
        {"method": "GET", "path": "/v1/models"},
        {"method": "GET", "path": "/props"},
    ],
}


def _probes(engines):
    """Every request of the engines named, once each, tagged with the engines it belongs to."""
    probes = []
    for engine, specs in engines.items():
        for spec in specs:
            same = [p for p in probes if (p["method"], p["path"]) == (spec["method"], spec["path"])]
            if same:
                same[0]["engines"].append(engine)
            else:
                probes.append(dict(spec, engines=[engine]))
    return probes


DEFAULT_KEYLESS = _probes(ENGINE_PROBES)

# A variable a model server reads its key from when the manifest writes it out
# rather than naming a secret.
KEY_VARIABLE = re.compile(r"(^|_)(API_?KEY|KEY|TOKEN)$")
ENV_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def _served(status):
    return status is not None and 200 <= status < 300


def _key_names(manifest):
    """The names a Package gives its units a key under: every secret, and every variable that reads as a key."""
    names = []

    def walk(node):
        if isinstance(node, dict):
            for key, value in node.items():
                if key == "secrets" and isinstance(value, list):
                    names.extend(str(v) for v in value)
                elif key == "environment" and isinstance(value, dict):
                    names.extend(str(k) for k in value if KEY_VARIABLE.search(str(k)))
                else:
                    walk(value)
        elif isinstance(node, list):
            for value in node:
                walk(value)

    walk((manifest or {}).get("deploy"))
    return [n for n in dict.fromkeys(names) if ENV_NAME.match(n)]


def _as_member(ctx, command):
    """A compound command as the member. as_member puts the runtime directory in
    front of the command, which the shell takes only before a simple command."""
    return hostops.as_member(ctx.root_alias, ctx.member, ctx.uid, "sh -c %s" % shlex.quote(command))


def _container_env(ctx, process, names):
    """The values these names have in the running containers of one Process, read as the member.

    This is the least the bench can use to learn the key the agent set. The
    member's surface never answers a secret's value, and the admin reads only
    their own, so the command goes through the root alias, but it runs as the
    member, with nothing but the member's own podman, and only the lines of
    the names asked leave the host. It reads what the Process was given, which
    is the outcome, rather than what the transcript says the agent sent.
    """
    pattern = "|".join(names)
    command = (
        "for c in $(podman ps -q --filter label=kitbash.id=%s); do "
        "podman inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $c; done "
        "| grep -E '^(%s)=' | sort -u" % (re.sub(r"[^A-Za-z0-9-]", "", process.get("id") or ""), pattern)
    )
    code, out, _ = _as_member(ctx, command)
    values = {}
    for line in (out or "").splitlines():
        name, _, value = line.partition("=")
        if name in names and value:
            values.setdefault(name, []).append(value)
    return values


def _keys(ctx, processes):
    """The keys the run set, as (name, value) pairs, or the reason none could be read.

    The value never goes into a result: a row is committed, the key is the
    member's, and a detail names only the variable it came from.
    """
    if not ctx.root_alias or not getattr(ctx, "uid", None):
        return [], "skipped: this check reads the key as the member through the round's root alias, and has no root alias or uid"
    keys = []
    declared = []
    for process in processes:
        package = process.get("package")
        if not package:
            continue
        names = _key_names(ctx.call("pkg_inspect", {"path": package}).get("manifest"))
        declared += names
        if not names:
            continue
        for name, values in _container_env(ctx, process, names).items():
            keys += [(name, value) for value in values]
    if keys:
        return list(dict.fromkeys(keys)), None
    if not declared:
        return [], "no unit declares a secret or a key variable, so there is no key to make a request with"
    return [], "no running container of this run holds %s" % ", ".join(sorted(set(declared)))


def http_refuses_without_key(ctx, params):
    """Every address this run made refuses a request with no key, and answers one with it.

    Reaching a model server is not the point; being refused without the key
    is, because an http Process is public and a model server that answers
    everyone is the trap the recipes exist for. Each address is asked on the
    paths of every engine, OpenAI's, Ollama's and llama-server's: a path that
    answers 2xx without a key fails the run, a path the engine does not have
    may answer 404, and at least one path has to answer 401 or 403. A second,
    open Process beside a guarded one fails the run too.

    Refusing is half of "only for me". The other half is that the member's
    own key gets through, so one request with the key the run set has to
    answer 2xx, and a server that refuses everyone fails.
    """
    candidates = _new_with_address(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    refused = _statuses({"status": params.get("status", [401, 403])})
    requests = params.get("requests") or DEFAULT_KEYLESS
    seen = []
    guarded = []
    for process in candidates:
        base = ctx.url_of(process).rstrip("/")
        answers = []
        for spec in requests:
            status, body = fetch(base + spec["path"], method=spec.get("method", "GET"), body=spec.get("body"))
            answers.append({"url": base + spec["path"], "method": spec.get("method", "GET"), "status": status,
                            "body": (body or "")[:120]})
            if status in refused:
                guarded.append((base, spec))
        seen += answers
        open_ones = [a for a in answers if _served(a["status"])]
        if open_ones:
            return {"passed": False, "detail": "%s %s answered %s without a key"
                    % (open_ones[0]["method"], open_ones[0]["url"], open_ones[0]["status"]), "tried": seen}
        if not [a for a in answers if a["status"] in refused]:
            return {"passed": False, "detail": "%s refused no request without a key, it answered %s"
                    % (base, ", ".join(str(a["status"]) for a in answers)), "tried": seen}
    # A key nobody set has to be refused where no key was: a proxy that only
    # asks whether there is a bearer at all lets everyone in.
    wrong = []
    bogus = "Bearer bench-%s" % uuid.uuid4().hex
    for base, spec in guarded:
        status, _ = fetch(base + spec["path"], method=spec.get("method", "GET"), body=spec.get("body"),
                          headers={"Authorization": bogus})
        wrong.append({"url": base + spec["path"], "method": spec.get("method", "GET"), "status": status})
        if status not in refused:
            return {"passed": False, "detail": "%s %s answered %s to a key nobody set"
                    % (wrong[-1]["method"], wrong[-1]["url"], status), "tried": seen, "wrong_key": wrong}
    keys, why = _keys(ctx, candidates)
    if why and why.startswith("skipped"):
        return {"passed": True, "detail": "%d requests without a key and %d with a wrong one, all refused; "
                "the request with the key was %s" % (len(seen), len(wrong), why),
                "seen": seen, "wrong_key": wrong, "key_probe": why}
    if not keys:
        return {"passed": False, "detail": "no request with the key could be made: %s" % why, "tried": seen}
    gets = [s for s in requests if s.get("method", "GET") == "GET"]
    # A proxy that guards only the chat path answers 404 or 405 to every GET,
    # so the one POST a client makes is the fallback. Its answer is not kept.
    posts = [s for s in requests if s.get("method") == "POST" and s["path"] == "/v1/chat/completions"]
    answered = []
    for process in candidates:
        base = ctx.url_of(process).rstrip("/")
        for name, value in keys:
            statuses = []
            for spec in gets + posts:
                if spec in posts and gets and [s for s in statuses if s not in (404, 405)]:
                    break
                status, _ = fetch(base + spec["path"], method=spec.get("method", "GET"), body=spec.get("body"),
                                  headers={"Authorization": "Bearer %s" % value})
                statuses.append(status)
                note = {"url": base + spec["path"], "method": spec.get("method", "GET"), "key": name, "status": status}
                answered.append(note)
                if _served(status):
                    return {"passed": True,
                            "detail": "%d requests without a key and %d with a wrong one, all refused; "
                            "%s %s with %s answered %s" % (len(seen), len(wrong), note["method"], note["url"], name, status),
                            "seen": seen, "wrong_key": wrong, "key_probe": note}
    return {"passed": False, "detail": "no request with the key answered 2xx, so the Process refuses everyone",
            "tried": seen, "with_key": answered}


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


def _post_entry(base, marker):
    """Write a marker through whichever path and shape the application takes, and find where it reads back."""
    for path in ENTRY_POSTS:
        for shape in ("json", "form"):
            if shape == "json":
                status, _ = fetch(base + path, method="POST", body=_entry_fields(marker))
            else:
                status, _ = fetch_form(base + path, _entry_fields(marker))
            if status and status < 400:
                found = _where_marker(base, marker, [path] + ENTRY_READS)
                if found:
                    return {"post": path, "shape": shape, "read": found}
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
        posted = _post_entry(base, marker)
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


def _dump_count(ctx, process, marker):
    """How many times a marker is in the data of a Postgres among the containers of one Process.

    Run as the member, with their own podman, in each container of the
    Process: the one that has pg_dumpall and a server on its socket is the
    Postgres, and only a count leaves the host. The official image trusts a
    connection on its own socket, and the password is given in case it does
    not.
    """
    command = (
        "for c in $(podman ps -q --filter label=kitbash.id=%s); do "
        "podman exec $c sh -c 'command -v pg_dumpall >/dev/null && "
        "PGPASSWORD=\"$POSTGRES_PASSWORD\" pg_dumpall --data-only -U \"${POSTGRES_USER:-postgres}\"' 2>/dev/null; "
        "done | grep -c '%s'" % (re.sub(r"[^A-Za-z0-9-]", "", process.get("id") or ""), re.sub(r"[^a-z0-9-]", "", marker))
    )
    _, out, err = _as_member(ctx, command)
    try:
        return int((out or "0").strip().splitlines()[-1]), err
    except (ValueError, IndexError):
        return 0, err


def entry_in_database(ctx, params):
    """An entry written through the application is in the Postgres of this run.

    A guestbook that keeps its entries in a file survives a restart as well
    as one that keeps them in Postgres, so surviving is not where they are.
    The check writes its own marker through the application, then dumps the
    data of every Postgres among the Processes this run made, inside its own
    container, and looks for the marker there.
    """
    if not ctx.root_alias or not getattr(ctx, "uid", None):
        return {"passed": False, "detail": "this check reads the database as the member and needs the round's root alias and uid"}
    candidates = _new_with_address(ctx, params)
    if not candidates:
        return {"passed": False, "detail": "no Process with an address was made by this run"}
    wait = params.get("wait", 30)
    tried = []
    for process in candidates:
        base = ctx.url_of(process).rstrip("/")
        marker = "bench-%s" % uuid.uuid4().hex[:12]
        posted = _post_entry(base, marker)
        if not posted:
            tried.append({"url": base, "detail": "no path took an entry that could be read back"})
            continue
        deadline = time.time() + wait
        while True:
            for holder in candidates:
                count, _ = _dump_count(ctx, holder, marker)
                if count:
                    return {"passed": True,
                            "detail": "an entry written to %s is in the Postgres of %s" % (base, holder.get("name")),
                            "seen": dict(posted, url=base, marker=marker, database=holder.get("name"))}
            if time.time() >= deadline:
                break
            time.sleep(5)
        tried.append(dict(posted, url=base, marker=marker,
                          detail="an entry %s took is in no Postgres of this run" % base))
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


# What git is told on every call on a member's folder. The folder is the agent's,
# and so is its .git/config: an fsmonitor, a hook or a pager it names runs with
# the rights of whoever runs git. The command runs as the member, so that is
# the member's own; these switch off what a status or a listing would start,
# and the system and global configuration are left out.
GIT_SAFE = ("GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_OPTIONAL_LOCKS=0 git "
            "-c core.fsmonitor=false -c core.hooksPath=/dev/null -c core.pager=cat -c safe.directory='*'")


def _git(ctx, path, arguments):
    """git on a folder of the member's, run as the member and never as root."""
    return _as_member(ctx, "%s -C %s %s" % (GIT_SAFE, shlex.quote(path), arguments))


def nothing_committed(ctx, params):
    """No database file is committed, and nothing is left uncommitted, in a Package this run made.

    Read with git itself, because tracked is git's word, and as the member,
    because the repository and its configuration are the agent's. A file the
    Process wrote is a failure once a commit holds it, and also while it sits
    in the Package untracked and not ignored: that is the uncommitted change
    pkg_build answers conflict to. A mounted folder outside the Package is
    not read here; whether it stops a build is what pkg_builds asks.
    """
    if not ctx.root_alias or not getattr(ctx, "uid", None):
        return {"passed": False, "detail": "this check runs git as the member and needs the round's root alias and uid"}
    packages = sorted({p["package"] for p in _candidates(ctx, dict(params, scope="new")) if p.get("package")})
    if not packages:
        return {"passed": False, "detail": "no Package was made by this run"}
    tracked = []
    dirty = []
    for package in packages:
        code, out, err = _git(ctx, package, "ls-files")
        if code:
            return {"passed": False, "detail": "git ls-files in %s failed: %s" % (package, err.strip()[:200])}
        tracked += ["%s/%s" % (package, f) for f in out.splitlines() if DATABASE_FILE.search(f)]
        code, out, err = _git(ctx, package, "status --porcelain -- .")
        if code:
            return {"passed": False, "detail": "git status in %s failed: %s" % (package, err.strip()[:200])}
        dirty += ["%s: %s" % (package, line.strip()) for line in out.splitlines() if line.strip()]
    if tracked:
        return {"passed": False, "detail": "%d database files are committed, first %s" % (len(tracked), tracked[0]),
                "tracked": tracked[:20]}
    if dirty:
        return {"passed": False,
                "detail": "%d uncommitted paths, which pkg_build refuses, first %s" % (len(dirty), dirty[0]),
                "uncommitted": dirty[:20]}
    return {"passed": True, "detail": "no database file is tracked and nothing is uncommitted in %s" % ", ".join(packages)}


def pkg_builds(ctx, params):
    """Every Package this run made still builds, through the member's own pkg_build.

    Asked after the restart, because what the Processes wrote since is what
    turns a Package into one pkg_build answers conflict to.
    """
    packages = sorted({p["package"] for p in _candidates(ctx, dict(params, scope="new")) if p.get("package")})
    if not packages:
        return {"passed": False, "detail": "no Package was made by this run"}
    built = []
    for package in packages:
        try:
            answer = ctx.call("pkg_build", {"path": package}, timeout=params.get("timeout", 900))
        except mcp.McpError as exc:
            return {"passed": False, "detail": "pkg_build %s refused: %s" % (package, exc.detail[:200]), "built": built}
        built.append({"package": package, "digest": answer.get("digest"), "commit": answer.get("commit")})
    return {"passed": True, "detail": "pkg_build answered a digest for %s" % ", ".join(packages), "built": built}


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
    "entry_in_database": entry_in_database,
    "nothing_committed": nothing_committed,
    "pkg_builds": pkg_builds,
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
