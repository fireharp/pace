# Pace

Pace keeps agent builds, tests, browsers, and models from exhausting your machine. It can queue, slow, or stop jobs when resources run low.

## Quick setup

One installer for macOS and Linux. It verifies the download and, on a Mac, asks whether you want the PaceBar menu bar app.

```sh
# Install the CLI
curl -fsSL https://github.com/fireharp/pace/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"

# Start in observe mode and check your machine
pace install --mode observe
pace status

# Run a job through Pace (from a Go project)
pace run -- go test ./...
```

Replace `go test ./...` with your own build or test command. To let Pace queue or stop heavy jobs, run `pace install --mode enforce`. See [PaceBar setup](#menu-bar-icon-macos) for unattended installs.

<details>
<summary>Download a standalone binary</summary>

| System | Apple silicon / ARM64 | Intel / AMD64 |
| --- | --- | --- |
| macOS | [pace_darwin_arm64](https://github.com/fireharp/pace/releases/latest/download/pace_darwin_arm64) | [pace_darwin_amd64](https://github.com/fireharp/pace/releases/latest/download/pace_darwin_amd64) |
| Linux | [pace_linux_arm64](https://github.com/fireharp/pace/releases/latest/download/pace_linux_arm64) | [pace_linux_amd64](https://github.com/fireharp/pace/releases/latest/download/pace_linux_amd64) |

For manual installation, run `mkdir -p ~/.local/bin`, then `install -m 755 <downloaded-file> ~/.local/bin/pace`. Continue from the `export PATH` line above. Archives, checksums and licenses are on the [releases page](https://github.com/fireharp/pace/releases/latest).

</details>

## How Pace works

- **Admission.** A job starts only when its expected memory footprint fits the current headroom. Otherwise it queues, and pace says why.
- **Ladder.** pace informs, then warns, then enforces: throttle, then freeze, then kill, always on the whole process group. A warning that is ignored is always followed by enforcement. At critical pressure the warning and the action happen together.
- **Feedback agents can act on.** Every queue, refusal, warning and kill is one `pace: VERB key=value` stderr line with a hint. A distinct exit code (200 refused, 201 killed, 202 pace error) and JSON status carry the same information.
- **No exemptions.** Processes started outside pace are watched and enforced too, and they lose first. On Linux pace writes their WARN and KILLED lines to their own stderr, so an agent whose command it kills reads why instead of a bare `Terminated`.
- **Deterministic.** Each decision is logged with its inputs, and `pace replay` re-runs it.

macOS on Apple silicon comes first. It runs as a per-user LaunchAgent with no root, cgo or kernel extension. Linux uses a systemd user unit and the same contract. The agent-facing contract is in [docs/CONTRACT.md](docs/CONTRACT.md); the design decisions are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Service setup

To build the CLI from source, run `go build -o ~/.local/bin/pace ./cmd/pace` with the Go version in `go.mod`.

`pace install` writes one of these files and starts the daemon:

- macOS: `~/Library/LaunchAgents/com.pace.daemon.plist`, with KeepAlive, a 1 s throttle, ProcessType Interactive, Nice −5 and `GOMEMLIMIT=32MiB`.
- Linux: `~/.config/systemd/user/pace.service`, with `Restart=always` and `RestartSec=1`.

Re-running it upgrades in place. `--dry-run` shows what it would write and run, and changes nothing. `pace uninstall` removes the service and keeps logs and config.

Stamp a version at build time with `go build -ldflags "-X main.version=v0.1.0" ./cmd/pace`.

### Linux specifics

Linux uses the same binary, contract and policy as macOS. Only sensing and enforcement differ:

- **Sensing.** Data comes from procfs and PSI. The footprint is anon + shmem + swap per process (`PACE_LINUX_FOOTPRINT`). Load comes from `/proc/schedstat`.
- **Job containers (cgroup v2).** The systemd unit sets `Delegate=yes`, and then every `pace run` job gets its own cgroup at `<delegation root>/pace/d-<hash>/job-<id>`. Inside a container:
  - Memory accounting is exact, and no descendant can escape the job.
  - In enforce mode the kernel holds a hard cap at budget × (1 + tolerance), set with `memory.max`. Turn it off with `enforce.hard_cap = false`. A job killed at that cap still gets `pace: KILLED policy=mem_budget` and exit 201.
  - Freeze uses `cgroup.freeze` and kill uses `cgroup.kill`.
  - The throttle adds `cpu.weight`/`io.weight` 1 and a `cpu.max` quota of 25 % of one CPU (`PACE_LINUX_THROTTLE_CPU_PCT`).
- **Fallback.** Without delegation (for example from an ssh session, whose scope is root-owned), or with `PACE_LINUX_CGROUP=0`, jobs are governed by process group, as on macOS. The `notes` field of `pace status` says which one applies and why.
- **Lines for processes outside pace.** Before it warns or kills a process group started outside `pace run`, the daemon writes the WARN or KILLED line and its hint to that group's stderr through `/proc/<pid>/fd/2`: a pipe, a Unix socket or a file, never a terminal (people in terminals see `pace status` or PaceBar). A file gets it when the process appends to it (as Claude Code's Bash tool does) or where the kernel lets pace share the process's own open file (`pidfd_getfd`). The write never blocks and never delays the signal by more than a few milliseconds. `[unmanaged] notify_stderr = false` turns it off; see [docs/CONTRACT.md](docs/CONTRACT.md) section 4. macOS cannot do this.
- **Throttle without root.** Without CAP_SYS_NICE the throttle raises nice to 10 (`PACE_LINUX_THROTTLE_NICE`), and a user cannot lower it again. `pace status` notes say so, and the THROTTLED line says so in `msg`.
- **Throttle under a container CPU quota.** In a container with a `cpu.max` quota of fewer CPUs than it may run on (`docker --cpus 2` on an 8-CPU host), nice and `SCHED_IDLE` alone do not reliably slow a job, because they only rank threads that share a CPU. pace does not claim to slow a job there: `pace status` notes explain why the throttle is limited, and, unless a job container caps the job with its own `cpu.max`, the THROTTLED line says `msg="throttle on this machine ... is limited by the container's CPU quota (nice acts per CPU only) ..."`.

## Quickstart

```sh
pace run -- go test ./...                         # admitted, queued, or refused with a reason
pace run --class build --mem 6G -- cargo build    # declare class and expected footprint
pace run --no-wait -- npm test || echo "exit $?"  # 200 instead of waiting
pace status                                       # headroom, pressure, jobs, suggested concurrency
pace status --json                                # the same, for scripts and agents
pace watch                                        # follow every governor event, machine-wide
```

Example `pace status`:

```
pace v0.1.0 (darwin)  mode=enforce  pressure=warn (kernel level 2)
memory    capacity 36.0G  avail 6.0G  reserved 3.0G  reserve 4.5G  headroom 0B  swap 7.7G
admitting no (pressure, headroom)
suggested concurrency 1 (browser=1 build=0 test=1)

RUNNING (1)
ID  CLASS  PRI  USED  PEAK  BUDGET  RESERVED  PROCS  AGE    STAGE      COMMAND
j3  build  5    5.5G  5.7G  8.0G    6.0G      14     2m30s  throttled  cargo build --release
```

What an agent sees when it is killed:

```
pace: WARN policy=mem_budget used=5.6G budget=6.0G next=kill in=20s job=j42
pace: hint: lower parallelism now or the job will be killed
pace: KILLED policy=mem_budget used=9.1G budget=6.0G job=j42 msg="used is over budget + tolerance (6.6G): killed at once, before the WARN's deadline"
pace: hint: retry with lower parallelism or request a larger budget
pace: EXIT code=201 source=pace job=j42
```

## Configuration

The config file is optional. It lives at `~/.config/pace/config.toml`, or wherever `$PACE_CONFIG` or `--config` points. Unknown keys are refused. Useful commands:

- `pace config example` prints every key with its default.
- `pace config check` validates the file.
- `pace config print` shows the effective values.
- SIGHUP or `POST /v1/reload` reloads the file.

```toml
mode = "enforce"            # observe | warn-only | enforce
reserve = "2G,12.5%"        # never handed out ("4G,30%" = the larger of the two)

[enforce]
warn_deadline = "20s"       # WARN -> action when nothing recovers
grace = "5s"                # SIGTERM -> SIGKILL (1s at critical pressure)

[classes.build]
estimate = "2G,15%"
budget = "4G,30%"
slots = "per:16G"           # one concurrent build per 16G of memory
wall_limit = "60m"
```

## Rollout

Switch enforcement on in three steps, on each new machine:

1. **observe** (`pace install --mode observe`). Every job is admitted. pace samples, warns, and writes the decision log. Run a day of real agent work, then check that a warning came before every stall and that `pace replay ~/Library/Logs/pace/decisions.jsonl` reports no mismatches.
2. **warn-only** (`pace install --mode warn-only`). The admission queue and warnings are live. Enforcement is only logged, as `would` actions in the decision log and INFO events on the feed.
3. **enforce** (`pace install --mode enforce`). The full ladder runs.

`pace doctor` warns while the mode is not `enforce`.

## Harness integration

Agents should not have to remember to use `pace run`.

```sh
pace shims install              # ~/.pace/shims: go, cargo, npm, pnpm, yarn, npx, pytest, make, tsc, uv, docker, ...
pace integrate claude --apply   # adds the hooks to ~/.claude/settings.json (backup kept; --remove undoes)
pace integrate codex --apply    # adds the hooks to ~/.codex/hooks.json; then review them once with /hooks in Codex
```

Without `--apply`, `pace integrate` only prints the snippet so you can merge it yourself.

- **Shims.** Each shim is a tiny sh script. Heavy forms (`go test`, `npm ci`, `cargo build`, `pytest`, ...) run through `pace run` with the right class. Everything else (`go version`, `npm view`) execs the real tool directly, in a few milliseconds. Inside a governed job (`PACE_JOB_ID` set) or with `PACE_SHIM_DISABLE=1`, shims always pass through. Commands stay unchanged, so permission rules still match.
- **Claude Code hooks.**
  - `SessionStart` puts the shim directory first on PATH for the session's Bash commands, through `CLAUDE_ENV_FILE`, and tells the agent how pace reports.
  - `PreToolUse` adds the governor status to the agent's context when memory pressure is at warn or above, with a hint that says what the current mode does (in observe nothing is queued or stopped; in warn-only new jobs queue but running ones are never stopped).
- **Codex hooks.** Codex has no way for a hook to change the PATH of later commands, and its `shell_environment_policy.set` does not expand `$PATH`. So the shims cannot route Codex's commands. Instead:
  - `SessionStart` tells the agent to run heavy commands as `pace run -- <cmd>`.
  - `PreToolUse` adds the same pressure context as for Claude Code, plus a reminder whenever a heavy command is about to run outside `pace run`.
- **Hook guarantees.** All hooks exit 0 and never block or rewrite a command.
- **Any other harness** that inherits PATH can use the shims directly: `export PATH=~/.pace/shims:$PATH`.

## All machines in one view (optional)

```sh
pace fleet add mini me@mini.example   # ssh destination: alias, user@host, Tailscale name or IP; checked once
pace fleet self mbp                    # this machine's name in the view (default: hostname)
pace fleet                             # one table: every machine's mode, pressure, memory, load, jobs, top process
pace fleet -v --watch                  # with jobs, warnings and the biggest processes, redrawn every 5s
pace fleet serve                       # the same as a page on http://127.0.0.1:7719 (and /fleet.json)
```

The fleet view is local-first and optional:

- Each daemon governs its own machine and knows nothing about the others.
- `pace fleet` reads this machine through its socket and every peer over ssh, with `pace status --json`. This works over Tailscale or a LAN, opens no port, and needs no new credentials, only ssh that works without a prompt.
- A peer that cannot be reached right now shows as `stale`, with its last snapshot and its age. No connection does not mean out of service.
- `pace-down` (the machine answered, its daemon did not) and `no-pace` are reported separately.
- To view it from a phone on the tailnet: `pace fleet serve --listen <this machine's Tailscale IP>:7719`, then open it by that IP or by its Tailscale name. Other host names are refused, to protect against DNS rebinding.

Peers are stored in `~/.config/pace/fleet.toml`. Add the same peers on each machine you want to look from.

### Menu bar icon (macOS)

The installer offers PaceBar in a Mac terminal. Answer `y` to install it in `~/Applications` and start it at login. Press Enter for the CLI alone. Unattended installs default to the CLI.

To install PaceBar later or choose it in a script:

```sh
curl -fsSL https://github.com/fireharp/pace/releases/latest/download/install.sh | sh -s -- --with-pacebar
```

Use `--cli-only` to skip the question in a terminal. The [PaceBar ZIP](https://github.com/fireharp/pace/releases/latest/download/PaceBar-macOS.zip) also includes the app and its install script.

The universal app requires macOS 13 or newer. It uses an ad-hoc signature and has no Apple notarization. To build it locally:

```sh
sh tray/macos/build.sh      # dist/PaceBar.app (needs Xcode's swiftc; universal, macOS 13+)
sh tray/macos/install.sh    # ~/Applications/PaceBar.app, started at login (LaunchAgent com.pace.bar)
```

PaceBar reads `pace fleet --json` every 10 seconds and only reads; it changes nothing. It works with or without peers.

- **The menu bar** shows one check mark while all is well. When something needs you it becomes a filled capsule that names the machine and the resource, for example `mini MEM` or `mbp DISK`:
  - amber for a warning: 80 % or more of memory in use, low disk space, a governor WARN (the kernel's pressure level at warn with less in use shows in the panel only);
  - red for memory pressure at critical, a job about to be killed (`mbp MEM KILL`), and a daemon that does not answer on this Mac (`mbp DOWN`).
- **Menu bar style** (a setting in the panel): `Status` is the default; `Bars` adds a memory bar per machine and `Text` shows memory in use per machine. Alerts look the same in all three.
- **The panel** shows, for each machine:
  - memory in use, headroom and load;
  - running and queued jobs;
  - the biggest processes and active warnings;
  - for a stale machine, its last snapshot and why it cannot be reached.
- **Buttons** open `pace fleet -v --watch` or `pace watch` in Terminal, or the log folder.
- **Notifications** (switch them off in the panel) come when memory in use passes 80 % or pressure turns critical, when a daemon stops answering, and for each new governor warning. A peer that cannot be reached is not one of them.
- **Removing it:** run the same install script with `--uninstall`.
- **Menu bar managers** such as Ice or Bartender put new icons in their hidden section. Move PaceBar to the visible part once.


## Testing

```sh
go test -race ./...                      # unit tests; never touch launchctl/systemctl or ~/.claude
go build -o bin/pacetest ./cmd/pacetest    # the load generator used by acceptance scripts
```

`pacetest` is the deterministic load tool:

| Command | What it does |
| --- | --- |
| `alloc --mem 2G [--step 256M --every 1s] [--hold 30s]` | Allocates and touches real memory (incompressible by default) |
| `spike --base 500M --peak 3G --at 20s` | Late spike, like a link step |
| `tree --n 4 --mem 512M` | Several allocating children in one process group |
| `escape --mem 1G` | A child that `setsid()`s out of the job's group, to test attribution |
| `burn --cpus 4` | Spins CPUs |
| `hang` | No CPU, no output (stuck detection) |
| `print --every 1s` | Steady output |
| `ignore-term` | Only SIGKILL stops it |
| `alloc --mem 1G --ignore-term` | A memory hog that only SIGKILL stops |
| `storm --depth 3 --fanout 4 --mem 8M [--setsid-leaves]` | A process tree that exists in full before it allocates |
| `probe --every 100ms` | Reports how late a timer fires (a stall check) |
| `exit 7`, `exit --signal TERM` | Exits with a code, or dies of a signal |

Allocations above 8G need `--i-know`. Every pacetest process exits by itself after `--max-life` (default 1h).

To simulate a small machine without exhausting the real one, combine three settings:

- `PACE_CAPACITY=8G`.
- `use_kernel_level = false`.
- `[unmanaged] scope_root_pid`, so only the test's own processes are candidates.

Point `PACE_HOME` and `PACE_SOCK` at a short temp directory to keep a test daemon apart from the real one. Socket paths must be at most 103 bytes. Acceptance scripts live in `scripts/`: `sh scripts/accept/run-all.sh` runs the whole suite on this machine, `sh scripts/docker-accept.sh` runs it on Linux in Docker (local `debian:bookworm-slim` image, never pulled), once as root with cgroup v2 job containers and once as an unprivileged user without them, and `sh scripts/docker-linux-test.sh` runs the Linux platform tests there.

## Safety notes

- pace signals only processes of the current user. It never touches operating-system processes (platform binaries under `/System`, `/usr/libexec`, ...), the daemon, or the `pace run` wrappers.
- Kills are always polite first: SIGTERM to the whole group, a grace period (5 s, or 1 s at critical pressure), then SIGKILL.
- The daemon is supervised. If it is down, `pace run` fails open, but within limits: at most 2 concurrent fail-open jobs, never into kernel-critical pressure, and always with a `pace: DEGRADED` line. Time without the daemon is a fault, and `pace doctor` reports it.
- `pace install` refuses binaries from `go run`'s temporary build cache, and warns about binaries in temporary directories.
- `pace integrate` changes a harness settings file only with `--apply` or `--remove`. It keeps everything that is not a pace hook and writes a backup first. `pace hook` never modifies anything. `pace shims` only replaces files that carry its marker.
- Under memory pressure the daemon stays small: `GOMEMLIMIT=32MiB`, no shelling out on the hot path, and its own pid is never a victim.

## License

[MIT](LICENSE)
