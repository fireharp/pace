# Notes for agents

Heavy commands here (make, npm, go, pytest, uv, ...) run under pace, a memory governor.
Lines starting with `pace:` on stderr explain queueing, warnings and kills; follow their `pace: hint:` lines.
Exit 200 = refused (retry after retry_after), 201 = killed by policy: the KILLED line's policy= says why and its hint what to do (mem_budget: lower parallelism or ask for a larger --budget; wall_clock: split the work or raise --timeout; stuck: check why the command hangs, e.g. waiting for input; pressure: retry when pace status shows headroom), 202 = pace usage error.
When a command dies with 137 or 143 and printed no `pace:` line, run `pace status`: its RECENT ALERTS section says whether pace stopped it.
