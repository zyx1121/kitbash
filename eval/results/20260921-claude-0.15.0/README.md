# 20260921-claude-0.15.0

One round of the M11 bench: 1 sentences, k=1, agent claude, model claude-sonnet-5, kitbash 0.15.0.

The member the round created was `bench-20260921-1403`. Every number below is read from the rows in this folder; the transcripts are under `runs/`.

## From a repository

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall |
|---|---|---|---|---|---|---|---|---|
| `repo-flask-redis` | 1/1 | 16 | 15 | 7 | 1 | 0.0 | 0.22 | 1 min |
| **total** | 1/1 | 16 | 15 | 7 | 1 | 0.0 | 0.22 | 1 min |

## Totals

| Class | sentences | pass^k |
|---|---|---|
| From a repository | 1 | 1 |
| **all** | 1 | 1 |

## The three readings of 4.5, restated

**The deployment conversation is gone.** 1 of 1 sentences passed every run. The answers asked 0 questions in all, and 0 runs ended by handing a decision back.

**What a sentence costs.** The mean run cost USD 0.22 and took 1 minutes. From nothing it was an amount this client does not report, from a repository USD 0.22, from a fault an amount this client does not report. M10 measured USD 0.28, 0.18 and 0.39 by hand on the three sentences this round keeps.

**The number to watch is the kitbash calls per sentence.** It is 7.0 here against M10's 8 to 14, with 15.0 tool calls in all and 1.0 of them failing. A fault was mitigated in no run.
