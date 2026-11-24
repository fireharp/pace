# pace agent contract

This is the contract between pace and the agents (and scripts) that run work through it, as implemented. The authoritative definitions live in `internal/proto` (`proto.go`: exit codes, verbs, line grammar, events; `api.go`: HTTP API and status types). If this document and the code disagree, the code wins and this document has a bug.

The meanings here are stable. A change to a verb, a key, a unit or an exit code is a breaking change, and it must land in `internal/proto` and in this file together.

## 1. Launching work

```
pace run [flags] [--] command [args...]
```

| Flag | Meaning | Default |
| --- | --- | --- |
| `--class C` | Job class: `build`, `test`, `browser`, `model`, `default` (or any configured class). An unknown class is governed as the default class, and pace prints `INFO msg="unknown class C, governed as default (classes: ...)"` | The config's default class |
| `--mem SIZE` | Declared expected peak footprint (the estimate) | Learned from earlier runs of the same command (one run per command for the class), else the class estimate. A learned value above what the machine can ever admit is not used: the command's observed peak if that fits, else `too_big` with a hint to declare `--mem`; for a class value, the class estimate |
| `--budget SIZE` | Memory budget. The job is warned near it and killed above it | The class budget |
| `--priority N` | 0..9, higher survives longer when victims are picked | The class priority (wrapped jobs default to 5, processes outside pace to 2) |
| `--timeout DUR` | Wall-clock limit (policy `wall_clock`) | The class limit |
| `--no-wait` | Refuse (exit 200) instead of queueing. Without it a new job is still refused at once (exit 200, `reason=disk`, with `retry_after`) while free disk space is critical: a disk does not free itself while a job waits | Off |
| `--queue-timeout DUR` | Give up after queueing this long (exit 200, `reason=queue_timeout`). `pace run` enforces it too: if the daemon has not decided 5 s past it, `pace run` refuses on its own, with `retry_after=5s` | `queue.timeout` (30m) |
| `--status-file F` | Write the final JSON job status to F | `$PACE_STATUS_FILE` |
| `--quiet`, `-q` | Suppress non-essential pace lines | Off |
| `--config F` | Config file | `$PACE_CONFIG`, else `~/.config/pace/config.toml` |

Flags stop at the first non-flag argument or at `--`: in `pace run npm test --mem 9G`, the `--mem 9G` belongs to npm.

The command runs in its own process group, which is what the governor accounts, freezes and kills. On Linux, when the daemon has a delegated cgroup v2 tree (`PACE_CGROUP_BASE`, or the auto-detected writable ancestor of the daemon's own cgroup), it also runs in a cgroup of its own (a job container) that every descendant stays in, whatever it does to its process group, session or environment: the governor accounts, caps, freezes and kills through it. Without one (`pace status` says why under `notes`) the process group is all there is. When the daemon has containers but this `pace run` cannot place the command in one (it runs in a cgroup it may not move processes out of, such as a root-owned ssh session scope, or `PACE_LINUX_CGROUP=0` is set), it prints `pace: INFO job=<id> msg="no job container (<why>): governed by process group"` once, and `pace status` `notes` lists the job among those without a container.

- stdin and a stdout that is a file of any kind (terminal, regular file, pipe, socket) are passed to the command as they are, so it can `fstat` or seek them. stderr goes through a relay pipe unless it is a terminal, so that pace lines land at the command's line boundaries; so do stdout and stderr together when they are the same non-terminal file (`>log 2>&1`), which keeps their order. Output that background descendants still write after the command exits keeps flowing: about 1 s after the exit, a relay still held open is handed to a `cat` process, and `pace run` exits. The exception is an exit with an EXIT line (code 200–202): the relay is then cut just before EXIT, so that EXIT stays the last stderr line, and a descendant's later write to it fails with EPIPE.
- INT, TERM, HUP and QUIT sent to `pace run` are forwarded to the job's process group. If the job is stopped (frozen by the governor, or by job control), it is also continued, so that the signal acts at once.
- Job control: when the command shares the caller's terminal in the foreground, it gets the terminal's foreground group (Ctrl-C and Ctrl-Z reach it). The terminal is found on stdin/stdout/stderr, or as `/dev/tty` when stdio is redirected, so that a password prompt on `/dev/tty` works. With `/dev/tty`, a `pace run` that is a job of its own (its own process group leader) gives the command the foreground at once. A `pace run` that shares its caller's process group (run from a script or a Makefile) gives it the terminal only when the command stops for terminal I/O, and only if the terminal is in canonical mode: a raw-mode terminal is driven by another program in that group (an agent harness's TUI), which keeps it. A command left stopped that way is reported once with `INFO msg="the command is stopped waiting for terminal input ..."`; it continues when `pace run` can hand it the terminal (on SIGCONT of `pace run`), and a signal sent to `pace run` still reaches it. Ctrl-Z (SIGTSTP) of the job, or a terminal stop of a job-leader `pace run`'s command, stops `pace run` with it, and continuing `pace run` continues the job.

Units:

- Sizes on the command line and in config are binary: `512M`, `4G`, `1.5G`, `2GiB` (1G = 2^30).
- Durations: `250ms`, `40s`, `2m30s`. A bare number means seconds.
- In stderr lines, sizes print as `5.6G` (one decimal, at 1G and above), `512M` or `64K`, and durations as `250ms`, `40s`, `2m30s` or `1h5m`.
- JSON always carries integers: `*_bytes` in bytes and `*_ms` in milliseconds (`ts` is Unix ms).

## 2. Exit codes

| Code | Meaning |
| --- | --- |
| Command's own code | The command was admitted and ran. Its code passes through unchanged |
| 200 | Refused. It will not run: `--no-wait`, a queue timeout, a footprint the machine can never give, or free disk space critical when it was submitted (`reason=disk`, also without `--no-wait`; retry after `retry_after`) |
| 201 | Killed by governor policy (`policy=` names it). A governor kill is 201, never 137 |
| 202 | pace usage or configuration error (bad flag, bad size, invalid config) |

The 200–202 range avoids 0–2, sysexits 64–78, 124–127, 128+signal (129–192) and 255. An 8-bit status cannot rule out a collision, because a command can itself exit 200. So whenever `pace run` exits with a code in 200–202, its **last stderr line** is an EXIT line that says who produced it:

```
pace: EXIT code=201 source=pace job=j42
pace: EXIT code=200 source=child job=j43          # the command itself exited 200
pace: EXIT code=202 source=pace msg="invalid value \"lots\" for flag -mem: invalid size \"lots\""
```

If the command dies of a signal that pace did not send (Ctrl-C, jetsam, an OOM kill), the wrapper re-raises the same signal on itself. The caller then sees exactly the status it would have seen without pace, so `$?` is 130 or 137 from a shell. A signal that was ignored when `pace run` started (under `nohup`, or `cmd &` in a script) cannot be re-raised; `pace run` then exits 128+N, which a shell shows as the same `$?`.

The daemon's record of a job agrees with these codes: after a governor kill, `pace status --job ID` shows `exit_code` 201 and `killed_by`.

Fail-open is not an exit code. If the daemon is unreachable, `pace run` prints `pace: DEGRADED reason=daemon_unreachable failopen_slot=N/M` and runs the command anyway, in fail-open slot N of M (`failopen.slots`, default 2). A daemon whose `/healthz` answers with a server error (its loop stalled) counts as unreachable, also while a job waits in its queue (checked every 5 s; two failures in a row, each a server error, no connection, or no answer within 10 s, the daemon's own stall limit). The command's own exit code passes through. A slot stays taken while the job's process group is alive, even after its `pace run` is gone. A group id that was reused by an unrelated group (a leader with another start time) does not hold it, and a `pace run` that ends after its job's group is gone clears the slot. No daemon knows a fail-open run, so its lines carry no `job=`; the command sees `PACE_JOB_ID=failopen-<pid of pace run>`, which only makes nested `pace run` calls pass through. When all slots are taken, `pace run` prints `QUEUED reason=failopen_full` and waits. With `--no-wait` it refuses at once with `reason=failopen_full`, or `reason=pressure level=critical` when memory pressure is critical. When the queue timeout runs out, it refuses with `reason=queue_timeout`. Each of these refusals carries `retry_after=5s`.

PATH shims (section 8) use the shell's codes when they cannot run anything. They exit 127 when the real tool is not on PATH outside the shim directory, and 126 when it cannot be executed.

Other subcommands:

| Command | Exit codes |
| --- | --- |
| `pace status` | 0 ok; 1 `--job` not found; 3 daemon unreachable; 202 usage |
| `pace watch` | 0 on Ctrl-C or end of stream; 3 daemon unreachable with `--no-reconnect`; 202 usage |
| `pace replay` | 0 all decisions match; 1 any mismatch; 202 unreadable input or usage |
| `pace config check` | 0 valid; 202 invalid |
| `pace doctor` | 0 nothing failed; 1 at least one FAIL |
| `pace install`, `pace uninstall`, `pace shims` | 0 ok; 1 failed; 202 usage |
| `pace hook ...` | Always 0. A hook never blocks the harness |

## 3. Message lines (stderr)

Every governor message is one line on stderr with a fixed prefix and stable `key=value` fields. pace never writes to the command's stdout. For a command started outside `pace run`, the daemon writes its WARN and KILLED lines to that command's own stderr on Linux (section 4).

```
line  = "pace: " VERB *(SP key "=" value) LF
hint  = "pace: hint: " free-text LF          ; only when the agent can do something
key   = [a-z][a-z0-9_]*
value = bare | quoted                       ; bare: no space, '"', '=' or control chars
                                            ; otherwise Go strconv.Quote form
```

A hint line always follows the line it belongs to. A message with no possible action carries no hint. Parse lines with `proto.ParseLine` or any `key=value` splitter that honours double quotes.

### Verbs

| Verb | When | Typical keys |
| --- | --- | --- |
| `QUEUED` | Admission is waiting for headroom, slots or lower pressure. The call blocks. The daemon sends it again whenever the position or the reason changes; `pace run` prints a changed reason at once, but a changed position at most once every 10 s (the latest position then; a wait that ends sooner ends with ADMITTED `waited=`), so a long queue costs an agent a few lines, not one per step. `reason=disk` queues only a job that was already waiting when free disk space became critical; a new submission is refused then | `reason free need position eta class job` (`reason=disk`: `reason free position eta class job`) |
| `ADMITTED` | Printed only if the job was queued first | `waited job` |
| `REFUSED` | Will not run; exit 200 follows. `reason=disk` refuses every new submission while free disk space is critical, with or without `--no-wait` | `reason free need retry_after job` (`reason=disk`: `reason free retry_after job`) |
| `WARN` | A policy is about to fire. `in=0ms` when it fires in the same step (critical pressure; in observe/warn-only mode, a budget exceeded past its tolerance). In observe and warn-only mode, a WARN whose `next` the governor will not carry out carries `mode=observe` or `mode=warn-only`: `next` and `in` still say what enforce mode would do, and the hint says what to do without threatening it. Without `mode`, `next` is real | `policy used budget next in mode job` (+hint) |
| `OK` | An earlier WARN cleared. A job still throttled or frozen when pressure returns gets a new `WARN` (with a full `in=`) before it is escalated again | `policy job` |
| `THROTTLED` / `UNTHROTTLED` | CPU/IO deprioritised / restored. On Linux without CAP_SYS_NICE the throttle also raises nice (default 10), which the kernel does not let a user lower again: after `UNTHROTTLED` the job, and children it started while throttled, keep that nice. With a job container whose cpu/io controllers are delegated, the throttle also sets the container's `cpu.weight` and `io.weight` to 1 (they rank it against the other pace jobs only) and caps it with `cpu.max` at 25 % of one CPU (`PACE_LINUX_THROTTLE_CPU_PCT`, 0 = off), which holds against every other process; `UNTHROTTLED` restores 100 and `max`. In a container whose cgroup-namespace root has a `cpu.max` quota of fewer CPUs than pace's CPU mask holds, nice and `SCHED_IDLE` only rank threads that share a CPU, so there the Linux throttle may not slow a job at all (it is applied as elsewhere; pace only says so). `pace status` lists the nice under `notes`, as it does a throttle that barely acts or is limited here (Linux `SCHED_BATCH` alone, such a container CPU quota, or no disk under the BFQ I/O scheduler, the only one that honours the idle I/O class). Then THROTTLED also carries `msg` saying so, e.g. `msg="throttle on this machine leaves nice 10 after UNTHROTTLED; see pace status notes"` or `msg="throttle on this machine leaves nice 10 after UNTHROTTLED and is limited by the container's CPU quota (nice acts per CPU only); see pace status notes"`; it does not call the throttle weak or limited for a job whose container caps it with `cpu.max`; on macOS (`PRIO_DARWIN_BG`, strong and reversible) it carries none | `policy job` (`msg`) |
| `FROZEN` / `THAWED` | SIGSTOP / SIGCONT on the job's process group. On Linux with a job container: `cgroup.freeze` (the processes then show state `S`, not `T`). A frozen job that something else continued (`pace run` when it lost the daemon or forwarded a signal, a `kill -CONT`, a write of `cgroup.freeze=0`) is stopped again, with `FROZEN ... msg="frozen again: it was continued while frozen"`: the daemon reads a frozen job container's `cgroup.freeze` back every tick, also after a restart | `policy job` (`msg`) |
| `KILLED` | Killed by policy; exit 201 follows for a wrapped job. On Linux with a job container in enforce mode the kernel caps the job at its budget plus tolerance (`enforce.hard_cap`); a job that jumps past that line is killed by the kernel and reported as `policy=mem_budget` with `msg="the kernel enforced the hard memory limit (cgroup memory.max)"`, which ends in `, before the WARN's deadline` too when a WARN's deadline had not passed yet. A `policy=mem_budget` kill the governor makes because `used` passed budget + tolerance comes at once, even before a WARN's announced `in=`, and says so: `msg="used is over budget + tolerance (6.6G): killed at once"`, with `, before the WARN's deadline` when a WARN's deadline had not passed yet (a kill at the WARN's deadline carries no `msg`). A unit's `mem_budget` WARN and KILLED (the operator's `unmanaged.budget`) carry hints of their own, which point to `pace run` and `unmanaged.budget`: a unit has no job to retry and no budget to request | `policy used budget job` or `unit pid comm` (+hint, `msg`) |
| `DEGRADED` | The daemon is unreachable or was lost mid-run; pace fails open. A job the lost daemon had frozen (FROZEN without THAWED, or stopped by SIGSTOP, and not continued since) is continued at once, with `INFO msg="continued the job its lost daemon had frozen"`, since nothing would thaw it. The freeze is on the whole process group: the job counts as continued only when no process of its group is stopped any more, so a leader running again (continued alone, or on macOS one that escaped the SIGSTOP in execve) does not leave the rest stopped. A SIGSTOP that the daemon sent just before it died is honoured even if `pace run` learns of the stop only after the stream error. A job that was not stopped when the daemon was lost gets no such line. A Linux job container also gets its throttle (`cpu.max`, weights) and `memory.max` lifted; a daemon that takes the job back re-applies what its state wants. On re-attach the daemon first sends the job's events the lost stream never delivered (`AttachRequest.last_event_ts`). A job that ends while the stream is lost is looked up on the daemon: if a restarted daemon killed it before the re-attach, `pace run` prints that KILLED line and exits 201 (EXIT line follows), and the daemon gets the exit report either way | `reason msg failopen_slot` |
| `EXIT` | Final line when the exit code is 200–202 | `code source job msg` |
| `INFO` | Informational, never actionable: observe/warn-only "would" actions (with `mode=`), pressure level changes, re-attach notes (`msg=reattached`), a command stopped waiting for a terminal `pace run` cannot hand it. In the feed, and printed by `pace run` when it concerns its own job | `msg` |
| `ACCEPTED` | Stream only. Never printed. The daemon's assigns the job id; the policy's, which follows, gives the resolved class, estimate, budget and priority | `job`; the policy's: `budget need priority class job` |

### Keys, in their fixed print order

| Key | JSON field | Unit / values |
| --- | --- | --- |
| `reason` | `reason` | `headroom`, `pressure`, `slots`, `disk`, `too_big`, `queue_timeout`, `daemon_unreachable`, `daemon_lost`, `failopen_full`; shims also emit `pace_missing` |
| `policy` | `policy` | `mem_budget`, `pressure`, `stuck`, `wall_clock`, `orphaned`, `cpu`, `disk` |
| `level` | `level` | `normal`, `warn`, `critical` |
| `used` | `used_bytes` | Size: the job's (or unit's) current footprint |
| `budget` | `budget_bytes` | Size |
| `free` | `free_bytes` | Size: the headroom left for this job at decision time, after running reservations, the reserve and the queued jobs ahead of it that wait for headroom. With `reason=headroom`, `free` is always below `need`. With `reason=disk` and `policy=disk` it is the free disk space instead (the lowest over the watched paths), and a `reason=disk` line has no `need`. On `policy=pressure` lines (WARN, THROTTLED, FROZEN, KILLED) and the `memory pressure` INFO it is the memory available on the machine now (`avail_bytes`), not headroom: under pressure the headroom is 0 by definition, and what is left is the useful figure |
| `need` | `need_bytes` | Size: the estimate that did not fit |
| `priority` | `priority` | Integer 0–9: the resolved priority (ACCEPTED only) |
| `position` | `position` | Integer: queue position (1 = next) |
| `eta` | `eta_ms` | Duration: estimated wait |
| `retry_after` | `retry_after_ms` | Duration: when a retry may succeed |
| `waited` | `waited_ms` | Duration spent queued |
| `idle` | `idle_ms` | Duration without CPU or output progress (`stuck`) |
| `elapsed` | `elapsed_ms` | Duration run so far (`wall_clock`) |
| `limit` | `limit_ms` | Duration: the limit that applies |
| `next` | `next` | `kill`, `freeze`, `throttle` or `refuse`: what follows if nothing changes |
| `in` | `in_ms` | Duration until `next` fires |
| `mode` | `mode` | `observe` or `warn-only`: the mode in which this line's `next` (or the INFO's "would" action) is not carried out. Absent when it is real (always in enforce mode; `next=refuse` in warn-only mode, where admission runs) |
| `code` | `code` | Integer (EXIT) |
| `source` | `source` | `pace` or `child` (EXIT) |
| `unit` | `unit` | `pg:<pgid>`: a process group started outside pace |
| `pid` | `pid` | Integer |
| `comm` | `comm` | Process name |
| `class` | `class` | Job class |
| `job` | `job` | Job id |
| `msg` | `msg` | Free text |
| `failopen_slot` | (status file: `failopen_slot`) | `N/M`: the fail-open slot taken. Only on the `DEGRADED` line of a fail-open run, printed last |

Example session:

```
$ pace run --class build -- cargo build --release
pace: QUEUED reason=headroom free=1.2G need=4.0G position=2 eta=40s class=build job=j42
pace: ADMITTED waited=38s job=j42
   Compiling ...
pace: WARN policy=mem_budget used=5.6G budget=6.0G next=kill in=20s job=j42
pace: hint: lower parallelism now or the job will be killed
pace: KILLED policy=mem_budget used=9.1G budget=6.0G job=j42 msg="used is over budget + tolerance (6.6G): killed at once, before the WARN's deadline"
pace: hint: retry with lower parallelism or request a larger budget
pace: EXIT code=201 source=pace job=j42
$ echo $?
201
```

In observe or warn-only mode nothing is killed, and the WARN says so:

```
pace: WARN policy=mem_budget used=5.6G budget=6.0G next=kill in=20s mode=warn-only job=j42
pace: hint: lower parallelism to stay within the budget (this mode only warns: the job will not be killed)
```

## 4. JSON event

Every line has a JSON form: the same content, one object (`proto.Event`). Optional numbers are omitted when absent, so `free_bytes: 0` means free was zero.

```json
{"v":1,"ts":1763800000000,"verb":"KILLED","job":"j42","class":"build","policy":"mem_budget",
 "used_bytes":9771050598,"budget_bytes":6442450944,
 "msg":"used is over budget + tolerance (6.6G): killed at once, before the WARN's deadline","hint":"retry with lower parallelism or request a larger budget"}
```

Fields: `v`, `ts`, `verb`, `job`, `unit`, `class`, `pid`, `pgid`, `comm`, `reason`, `policy`, `level`, `used_bytes`, `budget_bytes`, `free_bytes`, `need_bytes`, `priority`, `position`, `eta_ms`, `retry_after_ms`, `waited_ms`, `idle_ms`, `elapsed_ms`, `limit_ms`, `next`, `in_ms`, `mode`, `code`, `source`, `msg`, `hint`, and on the job stream only `mem_max_bytes` and `cgroup` (ADMITTED, never printed, not in the feed, alerts or job records: the hard memory limit and the path of the job's Linux container, which `pace run` creates with that limit).

Events appear in three places:

- The per-job stream (relayed by `pace run`).
- The job's status (`pace status --job ID --json`, `--status-file`).
- The machine-wide feed (`pace watch --json`, `GET /v1/events?follow=1`, `alerts.jsonl`).

A process started outside `pace run` gets its WARN and KILLED events on the feed and in the alerts file, identified by `unit=pg:<pgid>`, `pid` and `comm`, and on Linux also on its own stderr (next section).

### Lines on the stderr of a process started outside pace (Linux)

A process group started outside `pace run` (an agent's Bash tool running `./tools/gen-fixtures`) has no `pace run` to print its lines, so its command would end with only `Terminated` and exit 143. On Linux the daemon therefore writes the unit's WARN and KILLED lines, each followed by its hint, to the unit's own stderr itself, right before it acts on them: the KILLED line before the SIGTERM, so the process is still alive and whoever reads its stderr is still reading. They are the same lines, with the same grammar, as on the feed:

```
pace: WARN policy=pressure level=critical used=1.1G free=98M next=kill in=0ms unit=pg:4242 pid=4242 comm=gen-fixtures
pace: hint: free memory or finish soon; you are the next to be stopped
pace: KILLED policy=pressure level=critical used=1.1G free=98M unit=pg:4242 pid=4242 comm=gen-fixtures
pace: hint: retry when pace status shows headroom
```

- Which lines: WARN and KILLED of a unit, for every policy (`pressure`; `mem_budget` with the `unmanaged.budget` hints). Not THROTTLED, FROZEN, OK or THAWED, not observe/warn-only lines (`mode=`, INFO "would ..."), never a job's lines (`pace run` prints those). A pressure WARN at critical comes in the same step as the KILLED, just before it.
- Where: the fd 2 of every process the action reaches (the unit's members that pass the actuator's checks: never the daemon, a `pace run`, an operating-system or protected process, or one outside `scope_root_pid`), once per distinct target, so a pipe or file the members share gets one copy. A stderr a member inherited and still shares with another process (its parent's log file or pipe) is that process's stream too, so the lines show there as well. Only a target the process has open for writing, and only a pipe or FIFO, a regular file or an `AF_UNIX` socket: the lines reach pipes, files and Unix sockets, which is what agent harnesses read. Never a terminal (`/dev/pts/N`, `/dev/tty*`): a terminal may be shared with a protected or interactive process (the agent's own TUI), so people in terminals see `pace status` or PaceBar instead. Nor any other character device (`/dev/null`, consoles, serial ports), a pipe, FIFO or regular file the process has open for reading too (`O_RDWR`: it may be its own input or data, not a log), a file on a pseudo file system (procfs, sysfs, cgroupfs and the like), a socket that is not `AF_UNIX` (TCP, for example) or that is also the process's stdin (an inetd-style two-way channel). A write-only regular file on fd 2 is taken to be a log: a process started with stderr closed that then opens its output file write-only gets the lines in that file.
- Each line pair starts on a new line, so `^pace: ` matches it whatever the process was writing: a pipe or socket gets `\n` in front of it (in the same write; a reader may see an empty line before it), and a regular file gets one only when its last byte before the write position is not a newline.
- How: one write per line pair (at most 512 bytes a line, so a pipe takes it whole or not at all), non-blocking, never retried; every failure is ignored, and the signal is never held up by more than a few milliseconds (a full pipe or a hung file system just means no line). A regular file gets the lines at its end when the process opened it for appending (`>>`; Claude Code's Bash tool opens a command's output file that way), or through the process's own open file where the kernel lets the daemon share it (`pidfd_getfd`: not under Docker's default seccomp profile, nor Yama `ptrace_scope` ≥ 1 for a process the daemon did not start). A file the process did not open for appending gets nothing otherwise: its next write, at its own offset, would overwrite the line. A socket (a Node harness's stdio) is reachable only through `pidfd_getfd` too; a stream socket with a nearly full buffer may take only part of the text, which counts as not written (the part sent stays). All writes of a step stop one second after they started: what is left then is dropped, never written later. A dropped note (an earlier write still stuck, or the deadline) is a `stderr_note` fault in `pace status`.
- When stdout and stderr are the same file (`>log 2>&1`), the lines are in that file: pace never writes to a stdout that is not also the stderr.
- `unmanaged.notify_stderr = false` turns it off. macOS has no access to another process's file descriptors: there the feed, the alerts file and `pace status` (`recent_alerts`) remain the only places, as before.

A process group outside pace smaller than `min_victim` (64M) is not a unit on its own, but a swarm of them is not let off: under warn or critical memory pressure, when the candidates that rank no later than processes outside pace (over budget, or priority at most `unmanaged.priority`) cannot cover what must be freed but can together with the small groups, and the small groups together hold at least `min_victim`, those of at least `min_victim`/8 become victims too, after the big groups of their priority and before wrapped jobs of a higher priority, youngest first. A swarm that could not spare the wrapped job is left alone: it is never killed together with that job. A group below `min_victim`/8 is never a victim, nor is a small group that holds one of pace's own clients (the pace binary, matched by path or by its file name, as in `pace status`, or PaceBar). Only memory pressure acts on small groups: a group below `min_victim` never gets a new `mem_budget` warning (one warned while it was larger still gets its OK or its kill). `unmanaged_below_min_victim` in status counts them.

Some process groups are never a unit, so pace never throttles, freezes or kills them, whatever they hold (their memory still counts as used): the daemon's own group, the group of any `pace run` (the agent's or script's group that ran it), and with `scope_root_pid` the scope root's group (its descendants are units only in groups of their own). pgid 0 or 1 (init's group: what a PID 1 without job control, such as a container entrypoint script, starts) is never a unit as a whole: there each subtree below pid 1 (a process and its descendants in that group) is a unit of its own, `unit=pg:<root pid>`, except a subtree that holds the daemon or a `pace run` (the agent that ran it and its helpers), which is never one, and pid 1 and the scope root themselves. A terminal's shell (a process with a controlling terminal that leads its session, or that leads its own process group as the child of a session leader that is not the user's: the login shell that Terminal.app and iTerm2 start through root's `/usr/bin/login`) smaller than `min_victim` is never a member of a unit: a heavy command running in the shell's group without job control is still governed, signalled process by process, but the shell (and with it the user's terminal) is not.

## 5. Status JSON

`pace status --json` prints `GET /v1/status` (`proto.Status`) on one line:

| Field | Meaning |
| --- | --- |
| `v`, `ts`, `version`, `platform` | Schema version, Unix ms, daemon version, `darwin` or `linux` |
| `mode` | `observe`, `warn-only` or `enforce` |
| `capacity_bytes`, `avail_bytes` | Machine (or `PACE_CAPACITY`) memory and what is available now |
| `reserve_bytes` | Safety reserve that admission never hands out: the configured `reserve`, raised to the pressure warn line (`warn_avail_pct` of capacity) unless `admit_below_warn = true`. A job that stays within its estimate therefore cannot by itself cause warn pressure |
| `reserved_bytes` | Sum of running jobs' unconsumed reservations |
| `headroom_bytes` | What a new job may use now: avail − reserved − reserve |
| `pressure`, `kernel_level` | `normal`, `warn` or `critical`; the raw kernel level (1/2/4, 0 = unknown) |
| `swap_used_bytes`, `load1_milli`, `ncpu`, `disk_free_bytes` | Machine signals |
| `admitting`, `not_admitting_reasons` | Whether a new job of the default class would start now, and if not, why (reason values): `disk`, `pressure` (paused), `slots` (the default class has no free slot), `headroom` (no headroom, or a queued job it would wait behind does not fit) |
| `suggested_concurrency`, `suggested_by_class` | How many more heavy jobs fit now, overall and per class |
| `running`, `queued`, `recent` | Lists of `JobStatus` |
| `unmanaged` | Largest process groups started outside pace (`UnitStatus`): those of at least `min_victim`, and under memory pressure the smaller ones too |
| `unmanaged_below_min_victim` | `groups`, `used_bytes`, `min_victim_bytes`: every unmanaged process group (unit candidate) smaller than `min_victim`, and what they hold together, whether or not pace may act on it (below `min_victim`/8, or holding a pace client, it never does) and whether or not `unmanaged` lists it (absent when there is none). Many small processes can hold as much as one big one |
| `warnings` | Events for warnings that are currently active |
| `faults` | Daemon problems such as sampler errors or log gaps |
| `notes` | Standing platform limits: on Linux whether jobs run in cgroup v2 containers (and if not, why), a throttle that `UNTHROTTLED` cannot fully undo, or one that barely slows a job on this machine |

`JobStatus` fields: `id`, `class`, `state` (`queued`, `running` or `done`), `stage` (`throttled`, `frozen`, `terminating` or `killed`), `priority`, `argv`, `pid`, `pgid`, `nprocs`, `estimate_bytes`, `budget_bytes`, `reserved_bytes`, `used_bytes`, `peak_bytes`, `position`, `submit_ts`, `start_ts`, `end_ts`, `exit_code`, `killed_by` (a policy name) and `events`. `stage` and `killed_by` describe only what the governor really did: in `observe` and `warn-only` mode they stay empty (what would have happened is in the INFO "would ..." events and the decision log). `argv` is kept up to 4 KiB; a longer one ends with an `...[argv truncated: N more bytes]` element.

`UnitStatus` fields: `key` (`pg:<pgid>`), `pgid`, `leader_pid`, `comm`, `nprocs`, `used_bytes`, `start_ts`, `priority` and `stage` (empty for virtual enforcement, as for jobs).

`warnings` lists job and unit warnings the policy still holds, and machine-level ones while their condition holds: `policy=disk` while free disk space is below a threshold (until its `OK`), others while memory pressure is above normal. `next` and `in` say what follows now: once a pressure warning's deadline has passed and the target is throttled (or frozen), they are the freeze (or kill) and the time to it, as on the last THROTTLED (FROZEN) line, not the WARN's original `next=throttle`.

The job status file (`--status-file`, `PACE_STATUS_FILE`) holds the final `JobStatus` of that run, with the same keys. It is written just before `pace run` exits, and before its final EXIT line, however the run ends: also for a command that is not found (127) or not executable (126), and for a usage or config error (202; `--status-file` counts once it was parsed, else `PACE_STATUS_FILE`).

- `state` is `done`. `exit_code` is the code `pace run` returned. `killed_by` and `stage=killed` are set when this run ended by a governor kill; otherwise a done job has no `stage`.
- `events` holds the governor events this run printed (at most 32, oldest first).
- The daemon's view of the job supplies `class`, `priority`, `estimate_bytes`, `budget_bytes` and `peak_bytes` when the daemon answers at the end. Otherwise they come from the flags, and `peak_bytes` is 0. After a kernel kill at the job container's hard limit, `peak_bytes` is the kernel's peak (the KILLED line's `used`), in the daemon's record too.
- A fail-open run has no `id`.

The file adds these keys:

| Key | Meaning |
| --- | --- |
| `v` | Schema version (1) |
| `outcome` | `admitted`, `refused`, `killed`, `degraded` (ran without full governance), `error` (exit 202, or the command could not be executed) or `interrupted` (signalled before it ran) |
| `source` | `pace` or `child`: who produced `exit_code` |
| `signal` | The signal the command died of |
| `reason` | The REFUSED reason |
| `admit_ts` | When the job was admitted (Unix ms) |
| `output_bytes` | Bytes the command wrote. A stdout pipe or socket is passed through uncounted |
| `failopen_slot` | `N/M` for a fail-open run |
| `forwarded_signals` | Signals forwarded to the job |
| `error` | Why the daemon was not used, or why the command could not be executed |

Finished jobs stay queryable for 24 hours with `pace status --job ID`.

What an agent should do with it:

- Before fanning out parallel work, read `suggested_concurrency`.
- When `admitting` is false, wait or run less.
- After exit 200, retry no sooner than `retry_after`.
- After exit 201, act on the hint: lower parallelism, or pass a larger `--budget` or `--mem`.

## 6. Environment variables

pace was called gov before. Each setting below that pace reads also falls back to its old `GOV_*` name when the `PACE_*` one is unset, so existing setups keep working. The per-job variables `PACE_JOB_ID` and `PACE_STATUS_FILE` have no fallback.

| Variable | Read by | Meaning |
| --- | --- | --- |
| `PACE_JOB_ID` | Set by `pace run` on the command | The job id (`failopen-<pid>` for a fail-open run, which no daemon knows). Nested `pace run` and PATH shims see it and pass straight through. The daemon also uses it to attribute escaped descendants |
| `PACE_STATUS_FILE` | `pace run` | Default for `--status-file` |
| `PACE_CONFIG` | All | Config file path |
| `PACE_HOME` | All | Base directory for socket, state and logs; overrides the platform defaults (tests) |
| `PACE_SOCK` | All | Daemon socket path (at most 103 bytes) |
| `PACE_CAPACITY` | Daemon, config | Pretend the machine has this much memory, e.g. `8G` (tests) |
| `PACE_MODE` | Daemon, config | `observe`, `warn-only` or `enforce`; overrides the config. `pace install --mode` sets it for the service |
| `PACE_SHIM_DIR` | Shims, hooks | Shim directory (default `$PACE_HOME/shims`, else `~/.pace/shims`) |
| `PACE_SHIM_DISABLE` | Shims | Non-empty: shims always exec the real tool |
| `GOMEMLIMIT` | Daemon | Set to `32MiB` by the service files to keep the daemon small |

Default locations:

| | macOS | Linux |
| --- | --- | --- |
| Socket | `~/Library/Application Support/pace/run/pace.sock` | `$XDG_RUNTIME_DIR/pace/pace.sock` |
| Logs and decision log | `~/Library/Logs/pace/` | `$XDG_STATE_HOME/pace/` |
| Config | `~/.config/pace/config.toml` | Same |

## 7. HTTP API (unix socket)

HTTP/1.1 and JSON on the socket. The daemon creates a missing socket directory 0700; an existing one is never changed and must be owned by the user and not writable by group or others (else the daemon refuses to start). The socket is 0600 and the peer uid is checked. One daemon per `PACE_HOME`: `<home>/pace.lock` and `<socket dir>/pace.lock` are held for the daemon's life. Debug with `curl --unix-socket <sock> http://pace/v1/status`.

| Endpoint | Body | Returns |
| --- | --- | --- |
| `POST /v1/jobs` | `SubmitRequest` | NDJSON stream of `Event`: ACCEPTED, QUEUED*, then ADMITTED or REFUSED, then everything else until the job ends. Closing it means the wrapper is gone |
| `POST /v1/jobs/{id}/started` | `StartedRequest` (`cgroup`: the Linux job container the child was spawned into, if any; the daemon adopts it only when it is its own path for that job) | 204 |
| `POST /v1/jobs/{id}/progress` | `ProgressRequest` | 204 (heartbeat, about 1 s) |
| `POST /v1/jobs/{id}/exit` | `ExitRequest` (`oom_kill`, `peak_bytes`: the kernel killed the job at its container's own hard limit) | 204 |
| `POST /v1/jobs/{id}/attach` | `AttachRequest` (`last_event_ts`: the ts of the last event the previous stream delivered) | Stream (re-attach after a daemon restart): the job's later events first, then ACCEPTED and what follows |
| `GET /v1/jobs/{id}` | | `JobStatus` with events; 404 if unknown |
| `GET /v1/status` | | `Status` |
| `GET /v1/events?follow=1` | | NDJSON stream of every `Event` |
| `POST /v1/reload` | | 204 (re-read the config; same as SIGHUP) |
| `GET /metrics` | | Prometheus text |
| `GET /healthz` | | 200 `ok`; 503 with the reason when the loop has stalled or machine or process sampling has been failing (for 10 sample intervals, at least 10 s) (clients then treat the daemon as unreachable) |

## 8. Harness integration

Agents should not have to choose to use pace.

**PATH shims** (`pace shims install`) put one tiny sh script per heavy tool in the shim directory. Each script runs `exec <pace> shim-exec <name> "$@"`.

- `shim-exec` finds the real tool on PATH. It skips the shim directory, any other pace shim and pace itself, and it never resolves relative PATH entries.
- It execs the real tool directly (argv[0] preserved, about 2–10 ms) when any of these holds: `PACE_JOB_ID` is set, `PACE_SHIM_DISABLE` is set, or the command is not heavy.
- Otherwise it runs it as `pace run --class <class> -- <real> args...`.
- If the pace binary has disappeared, the shim prints `pace: DEGRADED reason=pace_missing` and runs the real tool anyway.

The heavy table (`internal/integrate/heavy.go`) is keyed on argv0 plus subcommand, skipping global flags such as `go -C dir` or `pnpm --filter x`. Info flags such as `--version` and `--help` are never heavy.

| Tool | Heavy forms | Class |
| --- | --- | --- |
| `go` | `build`, `run`, `vet`, `install` | build |
| `go` | `test` | test |
| `cargo` | `build`, `run`, `clippy`, `check`, `install` | build |
| `cargo` | `test`, `bench`, `nextest` | test |
| `npm`, `pnpm`, `yarn`, `bun` | `install`, `ci`, `add`, `build`, `exec` | build |
| `npm`, `pnpm`, `yarn`, `bun` | `test` | test |
| `npm`, `pnpm`, `yarn`, `bun` | `run <script>` | test\* → test; e2e/playwright → browser; else build |
| `yarn` (bare) | | build |
| `npx`, `pnpx`, `bunx`, `pnpm exec`/`dlx` | `playwright` | browser |
| `npx`, `pnpx`, `bunx`, `pnpm exec`/`dlx` | `vitest`, `jest`, `mocha` | test |
| `npx`, `pnpx`, `bunx`, `pnpm exec`/`dlx` | `tsc`, `next`, `vite`, `webpack`, `esbuild`, `turbo`, `nx` | build |
| `xcodebuild` | Any (`test` → test) except `-version`, `-list`, `-showsdks`, ... | build |
| `swift` | `build`, `run` | build |
| `swift` | `test` | test |
| `pytest`, `python -m pytest` | | test |
| `uv` | `run X` | X's class, else default |
| `uv` | `sync`, `pip`, `lock`, `build`, `tool run` | build |
| `make` | Any (`test*` or `check` target → test) except `-n` and `-q` | build |
| `tsc` | | build |
| `docker` | `build`, `buildx build`, `compose up`, `compose build`, `compose run` | build |
| `gradle`, `mvn` | Any (`test`, `check` or `verify` → test) | build |

**Harness hooks.** `pace integrate claude|codex` prints the snippet. `--apply` merges it into `~/.claude/settings.json` or `~/.codex/hooks.json`:
- Other keys and hooks are kept in order.
- Earlier pace hooks are replaced.
- A `<file>.pace-backup-<time>` copy is written.

`--remove` takes the pace hooks out again.

Claude Code:

- `pace hook claude-session-start` appends an idempotent `export PATH=<shimdir>:$PATH` to `$CLAUDE_ENV_FILE`, when shims are installed. It also prints one line of context: how pace works, what the exit codes mean (for 201, the remedy of each killing policy: the KILLED line's `policy=` and hint say which applies), and the current status. In `observe` and `warn-only` mode the line also says what that mode does not do (observe: nothing is queued, refused, throttled, frozen or killed; warn-only: new jobs queue or are refused, but no running job is throttled, frozen or killed).
- `pace hook claude-pre-tool-use` acts on Bash tool calls. When pressure is `warn` or `critical`, it prints:

  ```json
  {"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"pace: mode=enforce pressure=warn headroom=1.2G suggested_concurrency=1 admitting=false reasons=pressure running=3 queued=1\npace: hint: ..."}}
  ```

  The `pace: hint:` line depends on the mode and says only what that mode does:

  | Mode | `warn` | `critical` |
  |---|---|---|
  | `enforce` | heavy commands will queue or be refused | pace is stopping jobs now |
  | `warn-only` | heavy commands will queue or be refused; this mode stops no running job | the same, at critical |
  | `observe` | this mode only reports: heavy commands start at once and nothing is queued, refused or stopped | the same, at critical |

  For a heavy command it adds `pace: this command is heavy (class=…) and will wait for headroom` (admission is paused at `warn` and above), or in `observe` mode `… this mode (observe) starts it at once, so it adds to the pressure`.

  Otherwise it prints nothing.

Codex has no `$CLAUDE_ENV_FILE`, and it passes `shell_environment_policy.set` values literally (`$PATH` is not expanded), so the shims cannot reach its commands:

- `pace hook codex-session-start` prints one line of context (Codex adds plain stdout as developer context): run heavy commands as `<pace> run [--class …] -- <cmd>`, the exit codes, and the current status.
- `pace hook codex-pre-tool-use` adds what the Claude hook adds. Whenever a heavy command is about to run outside `pace run`, it also adds a reminder line, at any pressure and with the daemon down. A command counts as outside `pace run` when any simple command in it is heavy and not wrapped, including after `cd x &&`, `FOO=1` or `time`. Example:
  `pace: this command is heavy (class=test) but runs outside pace, so it gets no admission, warnings or fair share of memory; run heavy commands as: <pace> run --class test -- <command>`
- Codex runs new hooks only after the user reviews them once (`/hooks` in the Codex CLI).

All hooks always exit 0, never deny or rewrite a command, and finish within about 150 ms even when the daemon is down.

## Fleet view (optional)

`pace fleet` reads this machine through its socket. It reads every peer in `fleet.toml` (`$PACE_FLEET`, else next to the config) with `ssh -o BatchMode=yes <dest> pace status --json`, all in parallel, each within `timeout` (8 s). Daemons open no port and keep no fleet state; a machine governs itself whether or not the fleet answers.

| State | Meaning | Data shown |
| --- | --- | --- |
| `live` | Answered this round | Fresh status |
| `stale` | Not reachable now (ssh failed or timed out) | Last good snapshot and its age |
| `unreachable` | Not reachable and never seen | None |
| `pace-down` | The machine answered, but its daemon did not (`pace status` exit 3) | Last snapshot, if any |
| `no-pace` | The machine answered, but there is no pace binary (exit 127) | Last snapshot, if any |
| `error` | Any other answer | Last snapshot, if any |

Snapshots are cached per machine in `~/Library/Caches/pace/fleet` or `~/.cache/pace/fleet`. `pace fleet --json` prints `{"v":1,"ts":…,"machines":[{name,self,via,target,state,error,seen_ms,latency_ms,status}]}`, where `status` is the `pace status --json` object. `pace fleet` always exits 0 unless the fleet file is invalid (202). `pace fleet serve` serves the same view as a page and `/fleet.json`. It listens on 127.0.0.1:7719 by default, and it refuses to listen on all interfaces.
