# 20260920-codex-0.13.1

One round of the M11 bench: 15 sentences, k=1, agent codex, model gpt-6-astra, kitbash 0.13.1.

The member the round created was `bench-20260920-0100`. Every number below is read from the rows in this folder; the transcripts are under `runs/`.

## From nothing

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall |
|---|---|---|---|---|---|---|---|---|
| `link-shortener` | 1/1 | 16 | 12 | 10 | 3 | 0.0 | - | 5 min |
| `notes-wiki` | 1/1 | 22 | 18 | 14 | 4 | 0.0 | - | 6 min |
| `pdf-service` | 1/1 | 16 | 12 | 10 | 2 | 0.0 | - | 4 min |
| `todo-board` | 1/1 | 15 | 11 | 9 | 1 | 0.0 | - | 4 min |
| `weather-job` | 1/1 | 12 | 9 | 7 | 2 | 0.0 | - | 2 min |
| **total** | 5/5 | 16 | 12 | 10 | 2 | 0.0 | - | 4 min |

## From a repository

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall |
|---|---|---|---|---|---|---|---|---|
| `repo-flask` | 0/1 | 18 | 14 | 0 | 2 | 0.0 | - | 2 min |
| `repo-flask-redis` | 1/1 | 15 | 12 | 7 | 2 | 0.0 | - | 2 min |
| `repo-linkding` | 1/1 | 35 | 31 | 21 | 5 | 0.0 | - | 4 min |
| `repo-node` | 1/1 | 23 | 20 | 15 | 6 | 0.0 | - | 4 min |
| `repo-pgadmin` | 1/1 | 36 | 32 | 21 | 5 | 0.0 | - | 5 min |
| **total** | 4/5 | 25 | 22 | 13 | 4 | 0.0 | - | 3 min |

## From a fault

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall | time to mitigate |
|---|---|---|---|---|---|---|---|---|---|
| `fault-dependency` | 1/1 | 23 | 20 | 15 | 1 | 0.0 | - | 2 min | 2.1 min |
| `fault-image` | 1/1 | 18 | 15 | 12 | 0 | 0.0 | - | 1 min | 1.3 min |
| `fault-mount` | 1/1 | 40 | 35 | 28 | 4 | 0.0 | - | 5 min | 3.7 min |
| `fault-secret` | 1/1 | 20 | 17 | 13 | 2 | 0.0 | - | 2 min | 1.6 min |
| `fault-stopped` | 1/1 | 15 | 12 | 9 | 1 | 0.0 | - | 2 min | 1.3 min |
| **total** | 5/5 | 23 | 20 | 15 | 2 | 0.0 | - | 2 min | 2.0 min |

## Totals

| Class | sentences | pass^k |
|---|---|---|
| From nothing | 5 | 5 |
| From a repository | 5 | 4 |
| From a fault | 5 | 5 |
| **all** | 15 | 14 |

## The three readings of 4.5, restated

**The deployment conversation is gone.** 14 of 15 sentences passed every run. The answers asked 0 questions in all, and 0 runs ended by handing a decision back.

**What a sentence costs.** The mean run cost - and took 3 minutes. From nothing it was -, from a repository -, from a fault -. M10 measured USD 0.28, 0.18 and 0.39 by hand on the three sentences this round keeps.

**The number to watch is the kitbash calls per sentence.** It is 12.7 here against M10's 8 to 14, with 18.0 tool calls in all and 2.7 of them failing. A fault was mitigated in 2.0 minutes on average.
