# 20260919-claude-0.13.1

One round of the M11 bench: 15 sentences, k=1, agent claude, model claude-sonnet-5, kitbash 0.13.1.

The member the round created was `bench-20260919-2334`. Every number below is read from the rows in this folder; the transcripts are under `runs/`.

The member was removed at the end, and `users_remove` answered an internal 500 while doing it, twice. Both causes are in this host's internal log and both are findings of this round: the first attempt timed out, `kitbash-mcp`'s client deadline being shorter than a removal of a member holding seventeen Processes, which cancelled `userdel` halfway; the second failed to chown one file the mount fault had made immutable, having already archived the home, so the account was gone and the caller was told nothing had happened. The round now clears that flag before it removes its member.

Three sentences were run more than once and only the last run is kept as their row. `weather-job` first failed on a check of the bench's own: `proc_list` without a package answers one line per Process and that line carries the state and not the cron expression, so a registered job read as no job; the check now reads the Package's own listing when it sees a Process whose state is scheduled. `repo-filebrowser` was replaced by `repo-linkding`, because filebrowser/filebrowser was archived on 2026-09-01 and its final release prints a wind down notice and exits, so no agent could have passed it. `weather-job` was then run a third time with its first attempt's Package moved out of the member's home, because a sentence re-run against a member that already answered it is not a trial of anything.

## From nothing

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall |
|---|---|---|---|---|---|---|---|---|
| `link-shortener` | 1/1 | 32 | 31 | 15 | 3 | 0.0 | 0.61 | 3 min |
| `notes-wiki` | 1/1 | 23 | 22 | 15 | 2 | 0.0 | 0.57 | 3 min |
| `pdf-service` | 1/1 | 28 | 27 | 13 | 5 | 0.0 | 0.63 | 4 min |
| `todo-board` | 1/1 | 23 | 22 | 15 | 1 | 0.0 | 0.44 | 2 min |
| `weather-job` | 1/1 | 19 | 18 | 13 | 1 | 0.0 | 0.32 | 1 min |
| **total** | 5/5 | 25 | 24 | 14 | 2 | 0.0 | 0.51 | 3 min |

## From a repository

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall |
|---|---|---|---|---|---|---|---|---|
| `repo-flask` | 1/1 | 21 | 20 | 14 | 0 | 0.0 | 0.34 | 2 min |
| `repo-flask-redis` | 1/1 | 35 | 34 | 21 | 1 | 0.0 | 0.49 | 3 min |
| `repo-linkding` | 1/1 | 92 | 91 | 41 | 10 | 0.0 | 1.73 | 8 min |
| `repo-node` | 1/1 | 70 | 69 | 22 | 14 | 0.0 | 1.27 | 6 min |
| `repo-pgadmin` | 0/1 | 81 | 96 | 75 | 13 | 0.0 | 2.23 | 14 min |
| **total** | 4/5 | 60 | 62 | 35 | 8 | 0.0 | 1.21 | 6 min |

## From a fault

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall | time to mitigate |
|---|---|---|---|---|---|---|---|---|---|
| `fault-dependency` | 1/1 | 19 | 18 | 14 | 0 | 0.0 | 0.22 | 1 min | 0.6 min |
| `fault-image` | 1/1 | 18 | 17 | 13 | 0 | 0.0 | 0.22 | 1 min | 0.6 min |
| `fault-mount` | 1/1 | 25 | 24 | 18 | 2 | 0.0 | 0.45 | 3 min | 2.9 min |
| `fault-secret` | 1/1 | 15 | 14 | 10 | 0 | 0.0 | 0.18 | 1 min | 0.5 min |
| `fault-stopped` | 1/1 | 17 | 16 | 13 | 0 | 0.0 | 0.21 | 1 min | 0.8 min |
| **total** | 5/5 | 19 | 18 | 14 | 0 | 0.0 | 0.26 | 1 min | 1.1 min |

## Totals

| Class | sentences | pass^k |
|---|---|---|
| From nothing | 5 | 5 |
| From a repository | 5 | 4 |
| From a fault | 5 | 5 |
| **all** | 15 | 14 |

## The three readings of 4.5, restated

**The deployment conversation is gone.** 14 of 15 sentences passed every run. The answers asked 0 questions in all, and 1 run ended by handing a decision back (`notes-wiki`).

**What a sentence costs.** The mean run cost USD 0.66 and took 3 minutes. From nothing it was USD 0.51, from a repository USD 1.21, from a fault USD 0.26. M10 measured USD 0.28, 0.18 and 0.39 by hand on the three sentences this round keeps.

**The number to watch is the kitbash calls per sentence.** It is 20.8 here against M10's 8 to 14, with 34.6 tool calls in all and 3.5 of them failing. A fault was mitigated in 1.1 minutes on average.
