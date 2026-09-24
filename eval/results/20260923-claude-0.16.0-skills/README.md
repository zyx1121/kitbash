# 20260923-claude-0.16.0-skills

One round of the M11 bench: 2 sentences, k=1, agent claude, model claude-sonnet-5, kitbash 0.16.0.

The member the round created was `bench-20260923-1920`. Every number below is read from the rows in this folder; the transcripts are under `runs/`.

With `/org/skills` in place, as seeded from main at ff0aaa3. Both runs read a recipe before writing anything: `serve-ollama` for the chat model, `web-app` and then `postgres` for the guestbook.

## Answered by a recipe

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall | recipes read |
|---|---|---|---|---|---|---|---|---|---|
| `chat-model-private` | 1/1 | 17 | 16 | 12 | 2 | 1.0 | 0.31 | 2 min | `serve-ollama` |
| `guestbook-restart` | 1/1 | 33 | 32 | 19 | 2 | 0.0 | 0.55 | 3 min | `postgres`, `web-app` |
| **total** | 2/2 | 25 | 24 | 16 | 2 | 0.5 | 0.43 | 3 min |  |

## Totals

| Class | sentences | pass^k |
|---|---|---|
| Answered by a recipe | 2 | 2 |
| **all** | 2 | 2 |

## The three readings of 4.5, restated

**The deployment conversation is gone.** 2 of 2 sentences passed every run. The answers asked 1 question in all, and 0 runs ended by handing a decision back.

**What a sentence costs.** The mean run cost USD 0.43 and took 3 minutes. M10 measured USD 0.28, 0.18 and 0.39 by hand on the three sentences this round keeps.

**The number to watch is the kitbash calls per sentence.** It is 15.5 here against M10's 8 to 14, with 24.0 tool calls in all and 2.0 of them failing. A fault was mitigated in no run.
