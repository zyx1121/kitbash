# 20260923-claude-0.16.0-no-skills

One round of the M11 bench: 2 sentences, k=1, agent claude, model claude-sonnet-5, kitbash 0.16.0.

The member the round created was `bench-20260923-1927`. Every number below is read from the rows in this folder; the transcripts are under `runs/`.

The comparison: the same two sentences with `/org/skills` moved away for the round and moved back after it. Read by hand afterwards on the kept member, beside what the checks said: the guestbook is one Node unit writing `data/entries.json` through a mount, and its entries did survive `proc_stop` and `proc_run`, so it fails the Postgres unit M16 asks for, not persistence. Both Packages are left unbuildable: the Processes wrote into mounted folders with no `.gitignore`, and `pkg_build` of either answers `conflict`, uncommitted changes. The chat model is Ollama on its own port with no key, answering `GET /v1/models` with 200 to anyone, and its mount at `/root/.ollama` put 379 MB of weights and Ollama's generated `id_ed25519` private key into the member's Files.

## Answered by a recipe

| Sentence | pass^k | turns | tool calls | kitbash calls | tool errors | questions | cost USD | wall | recipes read |
|---|---|---|---|---|---|---|---|---|---|
| `chat-model-private` | 0/1 | 24 | 23 | 19 | 3 | 0.0 | 0.53 | 3 min | none |
| `guestbook-restart` | 0/1 | 19 | 18 | 13 | 2 | 0.0 | 0.38 | 2 min | none |
| **total** | 0/2 | 22 | 20 | 16 | 2 | 0.0 | 0.45 | 2 min |  |

## Totals

| Class | sentences | pass^k |
|---|---|---|
| Answered by a recipe | 2 | 0 |
| **all** | 2 | 0 |

## The three readings of 4.5, restated

**The deployment conversation is gone.** 0 of 2 sentences passed every run. The answers asked 0 questions in all, and 0 runs ended by handing a decision back.

**What a sentence costs.** The mean run cost USD 0.45 and took 2 minutes. M10 measured USD 0.28, 0.18 and 0.39 by hand on the three sentences this round keeps.

**The number to watch is the kitbash calls per sentence.** It is 16.0 here against M10's 8 to 14, with 20.5 tool calls in all and 2.5 of them failing. A fault was mitigated in no run.
