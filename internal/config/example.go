package config

// ExampleTOML returns a fully commented example configuration. Every
// setting line ("# key = value") shows the compiled-in default; uncommenting
// all of them changes nothing. Lines starting with "##" are explanations
// (and platform- or machine-dependent settings, shown with example values).
func ExampleTOML() string { return exampleTOML }

const exampleTOML = `## pace configuration: $PACE_CONFIG, else $XDG_CONFIG_HOME/pace/config.toml,
## else ~/.config/pace/config.toml. Every key is optional; unknown keys are an
## error. Reload with SIGHUP or POST /v1/reload.
##
## Sizes: "4G", "512M", "1.5G"; a percentage of capacity "30%"; or a comma
## list meaning the largest of the parts: "4G,30%".
## Durations: "250ms", "40s", "2m30s", "4h" (a bare number means seconds).
##
## Environment overrides (applied after this file):
##   PACE_HOME, PACE_SOCK, PACE_CAPACITY, PACE_MODE, PACE_CONFIG

## observe   = inform and warn only, every job admitted at once
## warn-only = admission queue and warnings; enforcement is logged, not done
## enforce   = the full ladder
# mode = "enforce"

## Simulate a smaller machine (tests). Percentages resolve against it, and
## use_kernel_level defaults to false while it is set.
## capacity = "8G"

## Memory never handed out by admission. Admission also never hands out
## memory below the pressure warn line (warn_avail_pct of capacity), so a job
## that stays within its estimate cannot by itself cause warn pressure.
# reserve = "2G,12.5%"
## Let admission hand out memory below the warn line (the reserve alone).
# admit_below_warn = false

## Fold the kernel pressure level (macOS) / PSI (Linux) into the pressure level.
## Default true, false when capacity is overridden.
## use_kernel_level = true

# sample_interval = "1s"
## Used while pressure is at warn or above, or a job is near its budget.
# fast_interval = "250ms"

## Paths. Defaults on macOS: home ~/Library/Application Support/pace, socket
## <home>/run/pace.sock, logs ~/Library/Logs/pace. On Linux: home
## $XDG_STATE_HOME/pace (~/.local/state/pace), socket
## $XDG_RUNTIME_DIR/pace/pace.sock (else <home>/run/pace.sock), logs <home>/logs.
## Setting home moves socket and logs under it. The socket path must be at
## most 103 bytes.
## home = "~/Library/Application Support/pace"
## socket = "~/Library/Application Support/pace/run/pace.sock"
## log_dir = "~/Library/Logs/pace"

[pressure]
## Pressure is warn when available memory falls below this share of capacity,
## critical below the second; warn must be greater than critical.
# warn_avail_pct = 15.0
# crit_avail_pct = 7.0
## Linux PSI: memory "some avg10" for warn, "full avg10" for critical.
# psi_warn_pct = 10.0
# psi_crit_pct = 5.0

[enforce]
## Time between WARN and enforcement (outside critical pressure).
# warn_deadline = "20s"
## SIGTERM to SIGKILL, normally and at critical pressure.
# grace = "5s"
# critical_grace = "1s"
## Escalation interval throttle -> freeze -> kill, normally and at critical.
# step = "2s"
# critical_step = "500ms"
## Pressure must stay normal this long before thaw / unthrottle.
# recover_hold = "5s"
## Wait for a lost wrapper to re-attach before reaping its job.
# orphan_grace = "10s"
## WARN mem_budget at this share of the budget.
# budget_warn_pct = 90.0
## Kill without waiting when used > budget * (1 + tolerance).
# tolerance_pct = 10.0
## Process groups smaller than this are not victims on their own. Under
## memory pressure, when nothing bigger of the same or lower priority covers
## what must be freed, a swarm of smaller groups outside pace (together at
## least min_victim) is, if it can cover the rest: those of at least
## min_victim/8, youngest first, before wrapped jobs of a higher priority.
## 'pace status' counts them.
# min_victim = "64M"
## Linux with cgroup v2 job containers, enforce mode: the kernel caps each
## job at budget * (1 + tolerance) (memory.max); a job that jumps past it is
## killed by the kernel and reported as KILLED policy=mem_budget.
# hard_cap = true

[unmanaged]
## Processes started outside the wrapper. Lower priority loses first.
# priority = 2
## Budget per process group ("none" = no budget; a group budget would kill browsers).
# budget = "none"
## Process or executable names whose process group is never acted on (the
## main process of a service that must survive, e.g. an agent orchestrator).
# protect = []
## Linux: a process group outside pace run has no pace run to print its WARN
## and KILLED lines, so pace also writes them (with their hints) to its
## processes' own stderr (a pipe, file or Unix socket; never a terminal),
## right before the signal. macOS cannot reach another process's stderr.
# notify_stderr = true
## Tests: only descendants of this pid are unmanaged candidates (0 = all).
# scope_root_pid = 0

[queue]
# timeout = "30m"

[cpu]
## Throttle jobs when the 1-minute load per core exceeds warn; lift below ok.
# warn_load_per_core = 1.5
# ok_load_per_core = 1.0

[disk]
## Default: "/" and your home directory.
## paths = ["/", "~"]
# warn_free = "10G"
## Admissions are refused below this.
# crit_free = "3G"

[learn]
## Reservation margin over the observed peak. Default 10 macOS, 5 Linux.
## margin_pct = 10.0
# history_size = 20
# min_samples = 3
## Command keys remembered; the least recently used is forgotten first.
# max_keys = 256

[failopen]
## Concurrent jobs allowed to run while the daemon is unreachable.
# slots = 2
## How long pace run retries the socket before failing open.
# wait = "2s"

[notify]
## macOS notification on KILLED.
# macos = false

[log]
## Decision log rotation (rotated files are gzipped).
# max_size = "64M"
# keep = 8
# heartbeat = "60s"

## Classes. A class here overrides only the fields it sets; a new name starts
## from the "default" class. Budgets are capped at capacity - reserve, and
## estimates at the budget. slots: "N", "0" (unlimited) or "per:16G"
## (one slot per 16G of capacity, at least 1). wall_limit and no_progress
## accept "none". priority 0..9, higher is more important.

# [classes.build]
# estimate = "2G,15%"
# budget = "4G,30%"
# slots = "per:16G"
# wall_limit = "60m"
# no_progress = "10m"
# priority = 5

# [classes.test]
# estimate = "1G,10%"
# budget = "2G,20%"
# slots = "per:8G"
# wall_limit = "30m"
# no_progress = "10m"
# priority = 5

# [classes.browser]
# estimate = "1.5G"
# budget = "3G,10%"
# slots = "per:8G"
# wall_limit = "30m"
# no_progress = "5m"
# priority = 4

# [classes.model]
# estimate = "4G,40%"
# budget = "60%"
# slots = "1"
# wall_limit = "4h"
# no_progress = "15m"
# priority = 6

# [classes.default]
# estimate = "1G,5%"
# budget = "2G,10%"
# slots = "0"
# wall_limit = "30m"
# no_progress = "10m"
# priority = 5

## A new class:
## [classes.lint]
## estimate = "512M"
## budget = "1G"
## slots = "4"
`
