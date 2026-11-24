package integrate

// ExampleConfig is printed by `pace config example`. Every setting is
// optional; the commented values are the compiled-in defaults (see
// docs/ARCHITECTURE.md). Uncomment a line to change it. Unknown keys are
// refused when the file is loaded.
const ExampleConfig = `# pace configuration: ~/.config/pace/config.toml (or $PACE_CONFIG, or --config).
# Every key is optional. Commented values are the defaults. Sizes accept
# "4G", "512M", a share of capacity "30%", or "4G,30%" (the larger of the two).
# Durations accept "250ms", "40s", "2m30s".

# Rollout: observe (inform and warn only) -> warn-only (admission queue,
# warnings, enforcement only logged) -> enforce (full ladder).
mode = "observe"

# capacity = "8G"            # pretend the machine has this much memory (tests)
# reserve = "2G,12.5%"       # never handed out by admission
# use_kernel_level = true    # fold the kernel pressure level into the level
# sample_interval = "1s"
# fast_interval = "250ms"    # used at warn/critical or when a job nears its budget

# home = ""                  # state dir; platform default (PACE_HOME overrides)
# socket = ""                # default <home>/run/pace.sock (PACE_SOCK overrides)
# log_dir = ""               # default ~/Library/Logs/pace (macOS)

# [pressure]
# warn_avail_pct = 15.0      # pressure=warn when available memory < 15% of capacity
# crit_avail_pct = 7.0       # pressure=critical below 7%
# psi_warn_pct = 10.0        # Linux PSI some avg10
# psi_crit_pct = 5.0         # Linux PSI full avg10

# [enforce]
# warn_deadline = "20s"      # WARN -> enforcement when nothing recovers
# grace = "5s"               # SIGTERM -> SIGKILL
# critical_grace = "1s"
# step = "2s"                # throttle -> freeze -> kill escalation at warn
# critical_step = "500ms"
# recover_hold = "5s"
# orphan_grace = "10s"       # wait for a lost wrapper before reaping its job
# budget_warn_pct = 90.0     # WARN mem_budget at 90% of budget
# tolerance_pct = 10.0       # kill at budget * 1.10 (5 on Linux)
# min_victim = "64M"         # smaller groups are victims only as a swarm under pressure

# [unmanaged]                # processes started outside 'pace run'
# priority = 2               # below wrapped jobs (5), so bypassers lose first
# budget = ""                # per process group; default none
# scope_root_pid = 0         # tests: only descendants of this pid

# [queue]
# timeout = "30m"

# [cpu]
# warn_load_per_core = 1.5
# ok_load_per_core = 1.0

# [disk]
# paths = ["/"]
# warn_free = "10G"
# crit_free = "3G"

# [learn]
# margin_pct = 10.0
# history_size = 20
# min_samples = 3

# [failopen]                 # when the daemon is unreachable
# slots = 2                  # concurrent fail-open jobs
# wait = "2s"                # reconnect wait before failing open

# [notify]
# macos = false              # macOS notification on KILLED

# [log]
# max_size = "64M"
# keep = 8
# heartbeat = "60s"

# Classes: estimate (expected peak), budget (kill line), slots ("0" =
# unlimited, "per:16G" = one per 16G of capacity), wall_limit, no_progress
# (stuck limit), priority (0..9, higher wins).
# [classes.build]
# estimate = "2G,15%"
# budget = "4G,30%"
# slots = "per:16G"
# wall_limit = "60m"
# no_progress = "10m"
# priority = 5
#
# [classes.test]
# estimate = "1G,10%"
# budget = "2G,20%"
# slots = "per:8G"
# wall_limit = "30m"
# no_progress = "10m"
#
# [classes.browser]
# estimate = "1.5G"
# budget = "3G,10%"
# slots = "per:8G"
# wall_limit = "30m"
# no_progress = "5m"
#
# [classes.model]
# estimate = "4G,40%"
# budget = "60%"
# slots = "1"
# wall_limit = "4h"
# no_progress = "15m"
#
# [classes.default]
# estimate = "1G,5%"
# budget = "2G,10%"
# slots = "0"
# wall_limit = "30m"
# no_progress = "10m"
`
