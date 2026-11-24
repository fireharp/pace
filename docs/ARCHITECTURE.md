# pace architecture and decisions

The following decisions define Pace’s resource policy and module layout. The shared types live in
`internal/proto` (contract, API), `internal/platform/platform.go` (OS interface), `internal/policy/types.go`
(pure policy core types), `internal/units`.

## Decisions

| Open decision | Choice | Why |
| --- | --- | --- |
| Job parent | The **wrapper** (`pace run`) spawns the job in its own process group (Setpgid) and stays its parent | Exit code + stdio pass through naturally; jobs survive a daemon restart (launchd restarts it in ~8 ms; measured) |
| IPC | HTTP/1.1 + JSON over a unix socket, stdlib only. Per-job NDJSON event stream on `POST /v1/jobs` | p99 108 µs; `curl --unix-socket` debuggable; stream close = wrapper liveness |
| Socket path | macOS `~/Library/Application Support/pace/run/pace.sock`; Linux `$XDG_RUNTIME_DIR/pace/pace.sock` (fallback `~/.local/state/pace/run`); `PACE_SOCK` overrides. Dir 0700, peer uid checked. Path must be ≤ 103 bytes (checked, clear error) | `$TMPDIR` may be cleaned |
| State/log dirs | macOS `~/Library/Application Support/pace/` (state.json and its durable fallback state.json.prev, history), logs `~/Library/Logs/pace/`; Linux `$XDG_STATE_HOME/pace/`. `PACE_HOME` overrides everything (tests) | |
| Config | TOML (`BurntSushi/toml`), unknown keys refused. `$PACE_CONFIG` or `~/.config/pace/config.toml`; optional, compiled-in defaults. SIGHUP / `POST /v1/reload` reloads | Human-editable, strict |
| macOS memory signals | Pure Go, `CGO_ENABLED=0`, `x/sys/unix` only: `kern.memorystatus_vm_pressure_level` (1/2/4), `kern.memorystatus_level` (% available), `vm.swapusage`, `hw.memsize`; per-process `ri_phys_footprint` via raw `SYS_PROC_INFO` (336, flavor 9 = PIDRUSAGE, RUSAGE_INFO_V4, offset 72) | 0.9 ms for 558 procs, no cgo, cross-compiles |
| Process list | `kern.proc.uid` via `SysctlKinfoProcSlice` (~90 µs); CPU from rusage `ri_user_time+ri_system_time` (mach ticks / `hw.tbfrequency`) | kinfo CPU fields are always 0 |
| Protected processes | pid == daemon, or wrapper pids; or (`CS_PLATFORM_BINARY` via csops AND exe path under `/System/`, `/usr/libexec/`, `/usr/sbin/`, `/sbin/`, `/Library/Apple/`); or `kill(pid,0)==EPERM`. Job membership overrides | Tested on 595 live processes: protects Finder/Dock/loginwindow, keeps zsh/ssh/node killable |
| Deprioritise | macOS `setpriority(PRIO_DARWIN_PROCESS, pid, PRIO_DARWIN_BG)` (reversible); Linux per thread: ioprio idle + `SCHED_IDLE` where its revert is permitted, else `SCHED_BATCH` + nice 10 (`PACE_LINUX_THROTTLE_NICE`; the nice is one-way unprivileged; with it 0 the throttle is reversible but weak; the idle I/O class acts only under BFQ: each reported in status `notes` and, short, in THROTTLED `msg`). Under a container CPU quota (`cpu.max` on the cgroup-namespace root, fewer CPUs than the affinity mask), the throttle is the same and reported as limited (status `notes`, THROTTLED `msg` unless the job's container holds it with `cpu.max`). With a Linux job container whose cpu/io controllers are delegated, additionally `cpu.weight`/`io.weight` 1 and `cpu.max` 25 % of one CPU (`PACE_LINUX_THROTTLE_CPU_PCT`; restored to 100 / `max`) | nice is one-way on macOS. Measured in Docker with the cpu controller on: a job cgroup is its own scheduling entity, so SCHED_IDLE, nice 10 and `cpu.weight=1` leave its share against a process outside pace at 50 %; `cpu.weight=1` cuts it to 1 % against another pace job; only the quota holds against everything (50 % → 25 %). Measured in a `cpu.max` 2-of-8-CPUs container without job containers (stress s10): nice and SCHED_IDLE weigh only threads on one runqueue while CFS bandwidth hands quota runtime to every CPU alike, so a nice-10 burner ran at 0.18–1.26× the per-thread rate of an unthrottled one. Pinning throttled threads to one CPU was tried and dropped: nothing lifts the pin if the daemon dies, and a restore without a record cannot tell pace's pin from the job's own | |
| Linux job containers | cgroup v2 `<delegation root>/pace/d-<hash of the socket path>/job-<id>`: one scope per daemon, so a second daemon of the user (a test daemon with its own `PACE_HOME`/`PACE_SOCK`) never lists, releases or removes the first one's containers; the daemon removes its scope at shutdown when it is empty (`PACE_CGROUP_BASE` pins the root `<delegation root>/pace`; `PACE_LINUX_CGROUP=0` disables). Only the daemon detects the tree: ADMITTED carries the job's path (`cgroup`) and hard cap (`mem_max_bytes`), job stream only. The wrapper creates that container with that cap (when it cannot, e.g. from a root-owned ssh session scope, it prints one INFO line and `pace status` `notes` lists the job as without a container) and spawns the child into it with `CLONE_INTO_CGROUP` (else spawn + attach); it reports the path with `started`/`attach`, and after a SIGKILL death reads `memory.events` (`oom` and `oom_kill`: a kill at the container's own limit, not a machine or ancestor OOM) and reports `oom_kill`, which the policy turns into `KILLED policy=mem_budget msg=...` (the wrapper prints it itself if the daemon's does not arrive). The daemon adopts a reported path only if it is its own path for that job, reads `cgroup.procs` + usage (anon + shmem + swap) each tick (attribution rule C: every process inside is the job's; usage replaces the per-process sum), sets `memory.max` = budget·(1+tolerance) in enforce mode only (`enforce.hard_cap`, default on; lifted in observe/warn-only, at job end, on shutdown), freezes with `cgroup.freeze` (members outside, if any, still SIGSTOP; for a job the state calls frozen `cgroup.freeze` is read back every tick and a container found thawed, by `pace run` on daemon loss or a forwarded signal, is frozen again with the "frozen again" FROZEN line), kills with `cgroup.kill` alongside the policy's signals, and removes the container once the job has ended and it is empty. Membership and emptiness are hierarchical: the processes of sub-cgroups a job created count as the job's, and removal removes the empty sub-cgroups first, in a single attempt on the loop (no wait), retried with a delay of 0.5 s doubling to 1 min when it fails. `pace run` that loses its daemon thaws its container and lifts its throttle and `memory.max` (fail-open); a re-attach makes the daemon re-apply freeze, weights and cap rather than trust what it last wrote. pace's own processes (a nested `pace run`'s wrapper, born in the outer job's container; the daemon itself) are moved out to `<scope>/pace-outside` every tick and right before a container-wide freeze or kill, so neither `cgroup.freeze`, `cgroup.kill` nor the kernel's `memory.oom.group` kill reaches them (if one cannot be moved, that job is frozen and killed by per-process signals, which never reach pace's own). A nested job that got no container of its own keeps its processes by pgid (rule 0 over rule C), and the outer job's usage is then its members' sum. A restarted daemon finds restored jobs' containers by job id and releases freeze/weights the state does not want. Without delegation (an ssh session scope is root-owned) everything falls back to process groups; `pace status` `notes` says which applies and why | the kernel's own accounting is exact (shared pages once, tmpfs included) and no descendant can escape it; with the cap the line where mem_budget kills at once holds between samples. Usage is not `memory.current − inactive_file`: that counts active page cache and kernel memory a kill does not free (measured 299–301M vs anon 294M for a 285M job's shell loop, past a 300M budget) |
| Budget tolerance | 10 % on both platforms (`enforce.tolerance_pct`) | Linux without a delegated tree samples a per-process sum like macOS |
| Sampling | 1 s normally; 250 ms when pressure ≥ warn or any job ≥ 80 % of budget (`sample_interval`, `fast_interval`) | sample costs ~1–3 ms |
| Reservation | `R = max(E, ceil(P·(1+m)))`, never shrinks while running; `Unconsumed = max(0, R−U)`; `Headroom = Avail − ΣUnconsumed − Reserve_eff`; released when the job ends. m = 10 % macOS, 5 % Linux. `Reserve_eff = max(reserve, warn line)` (`policy.EffectiveReserve`); `admit_below_warn = true` uses `reserve` alone | late spikes (linking) are what admission protects against. With the default `reserve` below the 15 % warn line (every machine of about 13 G or more), a job admitted into its declared headroom could reach warn pressure and be killed by the ladder while within estimate and budget |
| Estimate | declared > learned by command key > learned by class > class default. Learned ×(1+m), needs ≥ 3 samples: a key uses p90 of its last 20 peaks; a class uses the median, and learns one sample per command (the first recorded run of each key), so one heavy command cannot set it. A learned value above `capacity − Reserve_eff` is not used as is: a key falls back to its observed p90 peak (no margin) if that fits, else it is `too_big` with a hint to declare `--mem`; a class falls back to the class estimate. Never capped at the limit itself: that fits only on an idle machine, so the job would hold the queue behind it until its timeout. History keeps at most `learn.max_keys` (256) command keys, least recently used evicted first (class entries are kept); keys are at most 128 bytes | M2. A class mixes commands, so its p90 was set by its heaviest command, and a learned estimate above the limit refused every undeclared job of the class for good (refused jobs never teach a smaller value). State is cloned, hashed and logged every tick, so it must stay bounded |
| Pressure level | max(kernel level when `use_kernel_level`, avail thresholds: warn < 15 %, critical < 7 % of capacity; Linux PSI some10 ≥ 10 % warn, full10 ≥ 5 % critical) | kernel warn often comes late |
| Modes | `observe` (M0: admit all, warn only), `warn-only` (admission + warnings, stage 3 logged as `would`), `enforce` | rollout per working agreements |
| Admission | Queue order: priority desc, then arrival; slot-full classes are skipped; no backfill: a job that does not fit blocks every job behind it and claims the headroom (`free` for those behind is what is left after it). A refused `--no-wait` job leaves the queue and blocks nobody. `QUEUED` is re-sent when position or reason changes; `pace run` prints a new reason at once and a new position at most every 10 s (`timings.QueuedReprint`), the latest one. Unknown `--class`: default class plus an INFO line | big jobs are not starved by small ones; `free < need` must hold for `reason=headroom` |
| Pressure recovery | After `recover_hold` of normal pressure: OK to every pressure-warned target, then thaw/unthrottle one target per `step`. If pressure returns first, a target still throttled/frozen without a warning is re-WARNed with a full deadline before escalating | a target told OK must not be frozen or killed without a new warning |
| Victims | candidates = running jobs + unmanaged process groups ≥ `min_victim` (64M), not protected, not already dying. Small unmanaged groups (< `min_victim`, ≥ `min_victim`/8) join only under warn/critical pressure, and only when they make the difference: the candidates ranked no later than unmanaged groups (over budget, or priority ≤ `unmanaged.priority`) plus what is already being freed fall short of the deficit, those plus the small groups cover it, and the small groups together hold ≥ `min_victim`. They then rank after the big groups of their priority, so a swarm of small processes outside pace loses before a wrapped job of higher priority; a swarm that could not spare that job is left alone (it is never killed together with the job for nothing), and so is a lone small process. Only the pressure ladder acts on them (a group below `min_victim` is never a new `mem_budget` victim), and a small group holding pace's own clients (the pace binary, as in `pace status`, or PaceBar) is never one. Attribution lists the largest 64 big groups and, separately, the largest 64 small ones (handed to the policy only while pressure is above normal); status counts every small group in `unmanaged_below_min_victim`. Never a unit: the daemon's group, a wrapper's group, the scope root's group; pgid ≤ 1 as a whole (split by subtree below pid 1: each subtree is a unit `pg:<root pid>`, except one holding the daemon or a wrapper, and pid 1 and the scope root themselves); a terminal's shell < `min_victim` (a session leader with a controlling terminal, or a group leader on one whose parent is a session leader not listed for the user, as under Terminal.app's root `login`) is never a unit member (its group's other members still are, signalled one by one). Order: over budget → lowest priority → youngest → key. Pick until projected freed ≥ deficit | brief, fixed |
| Unmanaged priority | 2 (wrapped default 5), so bypassers lose first; no per-group budget by default (`unmanaged.budget`) | "an agent that bypasses the wrapper loses the job"; a group budget would kill browsers |
| Kill | SIGTERM to group + extra members, SIGCONT if frozen, grace (5 s; 1 s at critical), SIGSTOP stragglers, SIGKILL | |
| Unit lines on its own stderr | Linux only (`platform.StderrNotifier`; `unmanaged.notify_stderr`, default on). In the step that emits a unit's real WARN or KILLED (any policy; no `mode=`), the daemon writes that event's line and hint to `/proc/<pid>/fd/2` of the unit's members the actuator may reach (attribution's members this tick, through `unsafeFor`), leader first, before `execute` sends any signal. Per distinct target (device, inode) one write of both lines (each ≤ 512 bytes, the pair with a leading `\n` ≤ PIPE_BUF), starting on a new line: pipe/FIFO only when fd 2 is write-only, reopened `O_WRONLY\|O_APPEND\|O_NONBLOCK\|O_NOCTTY\|O_CLOEXEC`, checked with fstat, text with a leading `\n`; a regular file only when fd 2 is write-only (not `O_RDWR`) and not on a pseudo file system (fstatfs: procfs, sysfs, cgroupfs, debugfs, securityfs, ...), through `pidfd_getfd` (the process's own open file and offset), else reopened `O_APPEND` only when the process's fd has `O_APPEND` itself (fdinfo), with a leading `\n` only when the byte before the write position (pread) is not one; a socket only `AF_UNIX` (`SO_DOMAIN`) and not the process's fd 0 too (inetd-style channel), through `pidfd_getfd` with `sendmsg` `MSG_DONTWAIT\|MSG_NOSIGNAL` and a leading `\n`; a partial send counts as not written. Never a character device, so never a terminal. Only fds open for writing (fdinfo flags). On a goroutine with a hard 1 s deadline from its start, after which every remaining pid and note is dropped (no `/proc/<pid>` path is used late, so a late write never lands on a reused pid); the loop waits ≤ 5 ms; while a write is stuck (hung file system) later notes are dropped. Each drop is a `stderr_note` fault (at most one per 10 s, the rest counted into the next). No retry, every other error ignored | An unmanaged command has no wrapper stream: real agents (Claude Code) saw only `Terminated`/143 and guessed at an unknown supervisor. Before the SIGTERM the process is alive and its reader still reads. A file not opened for appending is left alone because its owner's next write at its own offset would overwrite the line with a fragment left over (measured in Docker, where seccomp denies `pidfd_getfd`); Claude Code's Bash tool opens a command's output file `O_APPEND` (measured, `W,AP`). An `O_RDWR` fd 2 may be a data file that landed on fd 2 (a process started with stderr closed), a pseudo-file-system file parses the text as a command, a non-`AF_UNIX` or two-way socket carries a peer's protocol, and a terminal may be shared with a protected or interactive process (a TUI): all left alone; people in terminals see `pace status` / PaceBar. macOS has no access to another process's descriptors, so it is unchanged |
| Exit codes | 200 refused, 201 killed by policy, 202 governor error. Final `pace: EXIT code=… source=pace|child` line when the code is in 200–202. Unrelated signal deaths are re-raised on the wrapper | 8-bit status can't avoid collisions; side channel disambiguates (raised with owner) |
| Fail open | wrapper retries the socket ≤ 2 s; then `DEGRADED reason=daemon_unreachable`, runs the command, limited to 2 concurrent fail-open jobs (flock slots), never into kernel-critical pressure (wrapper reads the sysctl itself) | Prime directive: daemon absence is a fault |
| Decision log | JSONL, one self-contained record per non-empty step + 60 s heartbeat: `{v,seq,ts,ver,cfg,params,state,in,out,state_hash}`. Rotate 64 MB × 8. Writer never blocks policy (bounded channel, `gap` record). `seq` strictly increases in file order across restarts (the daemon continues after the log's last seq, not only state.json's). `pace replay FILE` re-runs Step and diffs | deterministic replay |
| Harness integration | PATH shim dir (`pace shims install`) + Claude Code `SessionStart` hook appending to `CLAUDE_ENV_FILE`; PreToolUse adds status context only. Shims pass through when `PACE_JOB_ID` is set. Codex (no env file; `shell_environment_policy.set` is literal): SessionStart context says to use `pace run`, PreToolUse reminds when a heavy command runs outside it. `pace integrate … --apply` merges the hooks into the user's file with a backup | commands stay unchanged, permission rules unaffected; Codex measured: `set.PATH="x:$PATH"` arrives literally |
| Fleet view | client-side only: `pace fleet` reads the local socket and each peer over ssh (`pace status --json`, BatchMode, parallel, timeout); last good snapshot cached per machine, unreachable = `stale` with age; `serve` = page + JSON on localhost | no daemon port or credentials; works over Tailscale/LAN; local governance never depends on it |
| Alert channel | `GET /v1/events?follow=1` (`pace watch`), plus `alerts.jsonl`; optional macOS notification (off by default) | |
| Supervision | per-user LaunchAgent: KeepAlive, RunAtLoad, ThrottleInterval 1, ProcessType Interactive, Nice −5, GOMEMLIMIT 32MiB; systemd user unit on Linux | measured restart 8 ms |

## Layout

```
cmd/pace/            main: subcommands run|daemon|status|watch|fleet|replay|install|uninstall|shims|hook|integrate|config|doctor|version
cmd/pacetest/        test tool: alloc N, grow, burn CPU, hang, spawn escaped children, exit code
internal/units/     sizes and durations
internal/proto/     contract: exit codes, message grammar, events, HTTP API types
internal/platform/  Backend interface; darwin, linux, fake
internal/policy/    pure Step, attribution tracker (attrib.go), victim order, learning
internal/config/    TOML load/validate/defaults -> policy.Params; hash
internal/dlog/      decision log writer, rotation, replay
internal/daemon/    socket server, sampler loop, actuator, job table persistence, event fan-out, metrics
internal/wrapper/   pace run: submit, queue wait, spawn, stderr mux, heartbeat, exit codes, fail-open, re-attach
internal/integrate/ launchd/systemd install, PATH shims, Claude Code and Codex hooks, settings merge
internal/fleet/     fleet view: peers file, ssh fetch, snapshot cache, text/HTML rendering
scripts/accept/     end-to-end acceptance (POSIX sh): run-all.sh, overload.sh (M1, also M3 on Linux), m0, pressure, m2, robust, m4
scripts/            docker-accept.sh (the whole suite on Linux in Docker: root with job containers, uid 1000 without),
                    docker-linux-test.sh (the platform tests on Linux)
```

## Data flow per tick

1. Sampler: `Backend.SampleMachine`, `Backend.ListProcs(uid)`.
2. `policy.Tracker.Attribute(procs, jobs)` → `JobSample`s, `UnitSample`s, `ScopedUsedBytes` (sticky (pid,start) membership: Linux job container → pgid → ppid lineage → group-of-member → env tag; a job container's usage replaces the per-process sum).
3. Drain wrapper events into `Inputs.Events`.
4. `policy.Step(params, state, inputs)` → new state, output.
5. Actuator executes `Output.Actions` (not `Would`); events fan out to job streams, the feed, alerts.jsonl.
6. Decision log record; state persisted (atomic rename, fsynced off the loop) when changed.

The daemon runs Step on one goroutine. Testing never exhausts the real machine: `capacity` override +
`scope_root_pid` (only descendants of that pid are unmanaged candidates) + `use_kernel_level=false`.

Actuator invariants (`internal/daemon/actuate.go`), whatever the policy decided:

- An action for a job or unit reaches only processes attribution placed in that job or unit this
  tick (`attrib.Result.JobPIDs`, `UnitSample.PIDs`). A group holding anything else (the scope root
  or its ancestors, another job's members, an unrelated process now leading a job's reused pgid) is
  signalled member by member instead of with killpg.
- A unit's WARN and KILLED lines written to its members' stderr (Linux) go only to the stderr of
  processes an action for that unit may reach (a stderr they share with another process, such as an
  inherited pipe or file, reaches that process's stream too). The writes start before the step's
  signals, and the signals wait for them at most 5 ms; the writes stop at a hard 1 s deadline, after
  which no pid of that step is used.
- A job's leader is recorded exactly (pid and start) the first time the tracker sees the job; a
  leader not listed then is gone for good. A different process at that pid later marks the group
  reused, so it is never adopted, and a job with no members left and no wrapper is dropped. Only for
  jobs restored after a restart (running in the tracker's first tick) is the leader's start unknown;
  there it is checked against the job's `start_ms` within 3 s. The wrapper's `start_ms` is wall clock
  on Linux and the backend's start times are not, so nothing else compares the two (a clock step
  after the daemon starts must not leave new jobs ungoverned). Known limit: a job restored after a
  restart that followed a wall-clock step of more than 3 s since the job started is not re-adopted
  (fail-open).
- Destructive enforcement starts only on a fresh sample: when `SampleMachine` or `ListProcs` fails,
  the step gets `Inputs.Stale` and the policy neither starts nor escalates anything (no new
  throttle, freeze, kill, SIGKILL after grace, stuck warning); relaxing steps (thaw, unthrottle)
  still run. The decided stage transitions happen, with their signals and events, on the first
  fresh step, against the fresh process list. The actuator also holds any destructive action it
  still gets on a stale sample (fault `actuate_held`, a backstop) and `/healthz` turns 503 if it
  lasts.
- Throttling reaches every member of the target and is re-applied each tick to new members (children
  forked while throttled inherit background priority); likewise every member of a frozen target is
  stopped, including members found after the freeze. Everything backgrounded or stopped for a
  target is restored when the policy releases it, when the target leaves the state (job end), and on
  graceful shutdown, which also resets those stages in state.json and logs a `stop` record. The
  processes backgrounded or stopped per target (pid and start) are kept in state.json (`held`) and
  written before a freeze or throttle takes effect, so a daemon that replaces a crashed one releases
  exactly those processes, whether or not its fresh attribution places them in the target. That
  write is a temp file renamed into place with no fsync, on the loop: a crashed daemon does not lose
  the page cache, and the persister goroutine fsyncs it (file and directory) afterwards. So the loop
  never waits for the disk before a stop or throttle: the action lands in the step that decided it,
  the next ladder stage counts from then, and sampling continues on a busy disk. Each snapshot
  carries a sequence number, and an older one being written durably never replaces a newer one at
  the path. If that write fails, the persister writes the snapshot durably instead (`state_save`
  fault); if a durable write fails, the unsynced snapshot is still fsynced later, at the latest on
  shutdown. A record is forgotten only once a current process list shows its process gone.
  The trade-off is an OS crash or power loss inside the window between such a write and its fsync
  (seconds on a busy disk): the rename can reach the disk before the data (XFS, ext4 with
  `noauto_da_alloc` or `data=writeback`), leaving state.json empty or torn. So `state.json.prev` is
  always the last snapshot known durable: a hard link (a copy where links are unsupported) made by
  every durable write, every fsync of an unsynced one, and at startup from the state.json just
  loaded; the unsynced rename replaces only state.json. A state.json that does not parse is moved
  aside (`state.json.corrupt-<ms>`, a `state_load` fault) and the daemon loads `.prev` instead, so it
  keeps its jobs, id and log sequences and learned state, losing at most what changed in the window
  (the stopped processes did not survive the OS crash either). Without a usable `.prev` it starts
  fresh.
  A state.json that calls a target frozen but holds no records (written before they existed) seeds
  one: the group leader with the start the state knows; on the first current list, if that leader is
  still the same process and leads the group, the group's stopped members join it (a reused pid or
  pgid is never signalled). A `pace run` whose daemon stream is lost continues its job at once if the
  job was frozen (FROZEN without THAWED, or seen stopped by SIGSTOP), and a forwarded INT/TERM/HUP/QUIT
  to a stopped job is followed by SIGCONT so it acts. On re-attach it sends the ts of the last event it
  got, and the daemon replays the job's later events (a restarted daemon may have warned, frozen again
  or killed the job before the re-attach); a job that ended while lost is looked up
  (`GET /v1/jobs/{id}`), so a kill by a restarted daemon still ends in KILLED and 201, and the exit is
  reported.

Attribution at a job's start: step 1 applies `started` events that are still pending to the job list
it attributes against, so the job's first sample is already charged to the job. A protected wrapper's
own child (and its new process group) is never an unmanaged unit, even before its start is known; it
only counts towards scoped usage. Without both, a job that allocates fast can be picked as an
"unmanaged" pressure victim (killed with no KILLED line on its wrapper), or a lower-ranked job is
chosen instead of it. A daemonising descendant whose parent exits before any sample saw it is
attributed by its `PACE_JOB_ID` (rule 4: read only for processes started after the oldest running job,
whatever their parent, so orphans re-parented to a subreaper such as `systemd --user` count); one that
also drops `PACE_JOB_ID` cannot be attributed on macOS (no cgroups), nor on Linux without a job container: it
is an unmanaged unit. In a job container it stays the job's.
