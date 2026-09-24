"""The round read back: one table per class and the three readings of 4.5.

pass^k is the sentence every one of k runs passed, which is the number the
milestone asks for. Everything else is a mean over the runs of that sentence.
"""

import json
import os
import statistics

CLASSES = ["nothing", "repository", "fault", "recipe"]
CLASS_TITLES = {
    "nothing": "From nothing",
    "repository": "From a repository",
    "fault": "From a fault",
    "recipe": "Answered by a recipe",
}


def load_rows(folder):
    rows = []
    if not os.path.isdir(folder):
        return rows
    for name in sorted(os.listdir(folder)):
        if not name.endswith(".json") or name == "round.json":
            continue
        with open(os.path.join(folder, name)) as handle:
            rows.append(json.load(handle))
    return rows


def mean(values):
    values = [v for v in values if v is not None]
    return statistics.fmean(values) if values else None


def number(value, digits=0):
    if value is None:
        return "-"
    if digits:
        return ("%%.%df" % digits) % value
    return "%d" % round(value)


def tokens(row):
    """What a run read and wrote, for a client that reports no price."""
    usage = row.get("usage") or {}
    read = usage.get("input_tokens") or 0
    written = usage.get("output_tokens") or 0
    return read, written


def spend(rows):
    """What a run cost, in money, or in tokens for a client with no price."""
    cost = mean([r.get("cost_usd") for r in rows])
    if cost is not None:
        return "USD %.2f" % cost
    read = mean([tokens(r)[0] for r in rows])
    written = mean([tokens(r)[1] for r in rows])
    if not read:
        return "an amount this client does not report"
    return "{:,} tokens in and {:,} out".format(int(round(read)), int(round(written)))


def by_sentence(rows):
    """Rows grouped by sentence id, in the order the sentences were run."""
    grouped = {}
    for row in rows:
        grouped.setdefault(row["id"], []).append(row)
    for runs in grouped.values():
        runs.sort(key=lambda r: r.get("run") or 0)
    return grouped


def summarize(runs):
    """What one sentence did over its k runs."""
    k = len(runs)
    passed = sum(1 for r in runs if r.get("passed"))
    return {
        "id": runs[0]["id"],
        "class": runs[0]["class"],
        "k": k,
        "passed": passed,
        "pass_k": passed == k and k > 0,
        "turns": mean([r.get("turns") for r in runs]),
        "tool_calls": mean([r.get("tool_calls") for r in runs]),
        "kitbash_calls": mean([r.get("kitbash_calls") for r in runs]),
        "tool_errors": mean([r.get("tool_errors") for r in runs]),
        "questions_asked": mean([r.get("questions_asked") for r in runs]),
        "cost_usd": mean([r.get("cost_usd") for r in runs]),
        "wall_ms": mean([r.get("wall_ms") for r in runs]),
        "time_to_mitigate_ms": mean([r.get("time_to_mitigate_ms") for r in runs]),
        # The recipes any run of this sentence read, by folder name.
        "recipes_read": sorted({p.split("/")[3] for r in runs for p in r.get("recipes_read") or [] if p.count("/") >= 3}),
    }


def table(summaries, fault=False, recipe=False):
    head = "| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall |"
    rule = "|---|---|---|---|---|---|---|---|---|"
    if fault:
        head = head + " time to mitigate |"
        rule = rule + "---|"
    if recipe:
        head = head + " recipes read |"
        rule = rule + "---|"
    lines = [head, rule]
    for item in summaries:
        cells = [
            "`%s`" % item["id"],
            "%d/%d" % (item["passed"], item["k"]),
            number(item["turns"]),
            number(item["tool_calls"]),
            number(item["kitbash_calls"]),
            number(item["tool_errors"]),
            number(item["questions_asked"], 1),
            "-" if item["cost_usd"] is None else "%.2f" % item["cost_usd"],
            "-" if item["wall_ms"] is None else "%.0f min" % (item["wall_ms"] / 60000.0),
        ]
        if fault:
            value = item["time_to_mitigate_ms"]
            cells.append("-" if value is None else "%.1f min" % (value / 60000.0))
        if recipe:
            cells.append(", ".join("`%s`" % r for r in item.get("recipes_read") or []) or "none")
        lines.append("| " + " | ".join(cells) + " |")
    totals = [
        "**total**",
        "%d/%d" % (sum(i["passed"] for i in summaries), sum(i["k"] for i in summaries)),
        number(mean([i["turns"] for i in summaries])),
        number(mean([i["tool_calls"] for i in summaries])),
        number(mean([i["kitbash_calls"] for i in summaries])),
        number(mean([i["tool_errors"] for i in summaries])),
        number(mean([i["questions_asked"] for i in summaries]), 1),
        "-" if mean([i["cost_usd"] for i in summaries]) is None else "%.2f" % mean([i["cost_usd"] for i in summaries]),
        "-" if mean([i["wall_ms"] for i in summaries]) is None else "%.0f min" % (mean([i["wall_ms"] for i in summaries]) / 60000.0),
    ]
    if fault:
        value = mean([i["time_to_mitigate_ms"] for i in summaries])
        totals.append("-" if value is None else "%.1f min" % (value / 60000.0))
    if recipe:
        totals.append("")
    lines.append("| " + " | ".join(totals) + " |")
    return "\n".join(lines)


def by_class_spend(rows):
    """What a run cost per class, for the classes this round ran and no other.

    A round of one class, the recipe sentences for instance, would otherwise
    say the other three cost "an amount this client does not report", which
    reads as a measurement of runs that never happened.
    """
    parts = []
    for name in CLASSES:
        ran = [r for r in rows if r["class"] == name]
        if ran:
            parts.append("%s it was %s" % (CLASS_TITLES[name].lower(), spend(ran)))
    if len(parts) < 2:
        return ""
    text = ", ".join(parts[:-1]) + " and " + parts[-1]
    return " " + text[0].upper() + text[1:] + "."


def readings(rows, summaries):
    """The three readings of PLAN 4.5, restated with this round's numbers."""
    everything = list(summaries.values())
    asked = sum(r.get("questions_asked") or 0 for r in rows)
    handed_back = [r["id"] for r in rows if r.get("asks_user")]
    nothing = [s for s in everything if s["class"] == "nothing"]
    repository = [s for s in everything if s["class"] == "repository"]
    fault = [s for s in everything if s["class"] == "fault"]
    passed = sum(1 for s in everything if s["pass_k"])
    cost = mean([r.get("cost_usd") for r in rows])
    calls = mean([s["kitbash_calls"] for s in everything])
    mitigations = [s["time_to_mitigate_ms"] for s in fault if s["time_to_mitigate_ms"] is not None]
    lines = [
        "## The three readings of 4.5, restated",
        "",
        "**The deployment conversation is gone.** %d of %d sentences passed every run. "
        "The answers asked %d question%s in all, and %d run%s ended by handing a decision back%s."
        % (
            passed,
            len(everything),
            asked,
            "" if asked == 1 else "s",
            len(handed_back),
            "" if len(handed_back) == 1 else "s",
            "" if not handed_back else " (%s)" % ", ".join("`%s`" % i for i in sorted(set(handed_back))),
        ),
        "",
        "**What a sentence costs.** The mean run cost %s and took %s.%s M10 measured USD 0.28, 0.18 "
        "and 0.39 by hand on the three sentences this round keeps."
        % (
            spend(rows),
            "-" if mean([r.get("wall_ms") for r in rows]) is None else "%.0f minutes" % (mean([r.get("wall_ms") for r in rows]) / 60000.0),
            by_class_spend(rows),
        )
        + ("" if cost is not None else " This client reports no price, so what a run cost is its own token count."),
        "",
        "**The number to watch is the kitbash calls per sentence.** It is %s here against M10's 8 to 14, "
        "with %s tool calls in all and %s of them failing. A fault was mitigated in %s."
        % (
            number(calls, 1),
            number(mean([s["tool_calls"] for s in everything]), 1),
            number(mean([s["tool_errors"] for s in everything]), 1),
            "no run" if not mitigations else "%.1f minutes on average" % (mean(mitigations) / 60000.0),
        ),
    ]
    return "\n".join(lines)


def render(folder, state=None):
    rows = load_rows(folder)
    if not rows:
        return "# No rows in %s\n" % os.path.basename(folder)
    grouped = by_sentence(rows)
    summaries = {name: summarize(runs) for name, runs in grouped.items()}
    state = state or {}
    agents = sorted({r.get("agent") for r in rows if r.get("agent")})
    models = sorted({r.get("model") for r in rows if r.get("model")})
    versions = sorted({r.get("kitbash_version") for r in rows if r.get("kitbash_version")})
    members = sorted({r.get("member") for r in rows if r.get("member")})
    k = max(s["k"] for s in summaries.values())
    lines = [
        "# %s" % os.path.basename(folder),
        "",
        "One round of the M11 bench: %d sentences, k=%d, agent %s, model %s, kitbash %s."
        % (len(summaries), k, ", ".join(agents) or "-", ", ".join(models) or "-", ", ".join(versions) or "-"),
        "",
        "The member the round created was `%s`. Every number below is read from the rows in this "
        "folder; the transcripts are under `runs/`." % ", ".join(members) or "-",
        "",
    ]
    if state.get("note"):
        lines += [state["note"].strip(), ""]
    for name in CLASSES:
        items = [s for s in summaries.values() if s["class"] == name]
        if not items:
            continue
        items.sort(key=lambda s: s["id"])
        lines += ["## %s" % CLASS_TITLES[name], "", table(items, fault=(name == "fault"), recipe=(name == "recipe")), ""]
    total = len(summaries)
    passed = sum(1 for s in summaries.values() if s["pass_k"])
    lines += [
        "## Totals",
        "",
        "| Class | sentences | pass^k |",
        "|---|---|---|",
    ]
    for name in CLASSES:
        items = [s for s in summaries.values() if s["class"] == name]
        if items:
            lines.append(
                "| %s | %d | %d |" % (CLASS_TITLES[name], len(items), sum(1 for s in items if s["pass_k"]))
            )
    lines += ["| **all** | %d | %d |" % (total, passed), ""]
    lines += [readings(rows, summaries), ""]
    return "\n".join(lines)


def write(folder, state=None):
    text = render(folder, state)
    path = os.path.join(folder, "README.md")
    with open(path, "w") as handle:
        handle.write(text)
    return path
