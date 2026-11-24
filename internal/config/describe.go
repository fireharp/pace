package config

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/units"
)

// Describe is a human summary of the effective configuration
// (`pace config print`). It is stable for a given Resolved.
func (r Resolved) Describe() string {
	p := r.Params
	var b strings.Builder
	src := r.Source
	if src == "" {
		src = "defaults"
	}
	fmt.Fprintf(&b, "pace configuration (source: %s, platform: %s)\n", src, r.Platform)
	tw := tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
	row := func(k, f string, a ...any) { fmt.Fprintf(tw, "  %s\t%s\n", k, fmt.Sprintf(f, a...)) }
	ms := units.FormatMS
	by := units.FormatBytes
	pm := func(v int64) string { return strconv.FormatFloat(float64(v)/10, 'f', -1, 64) + "%" }
	onoff := func(v bool) string {
		if v {
			return "on"
		}
		return "off"
	}

	row("mode", "%s", p.Mode)
	if p.CapacityBytes > 0 {
		row("capacity", "%s (override; machine %s)", by(r.CapacityBytes), by(r.MachineBytes))
	} else {
		row("capacity", "%s (machine)", by(r.CapacityBytes))
	}
	if eff := policy.EffectiveReserve(p, r.CapacityBytes); eff != p.ReserveBytes {
		row("reserve", "%s (admission keeps %s: the warn line)", by(p.ReserveBytes), by(eff))
	} else {
		row("reserve", "%s (never handed out by admission)", by(p.ReserveBytes))
	}
	row("kernel level", "%s", onoff(p.UseKernelLevel))
	row("sampling", "%s, %s under pressure", ms(r.SampleMS), ms(r.FastMS))
	row("home", "%s", r.Home)
	row("socket", "%s", r.Socket)
	row("logs", "%s (rotate at %s, keep %d, heartbeat %s)", r.LogDir, by(r.LogMaxBytes), r.LogKeep, ms(r.HeartbeatMS))
	row("pressure", "warn when available < %s, critical < %s; PSI warn some10 >= %s, critical full10 >= %s",
		pm(p.WarnAvailPermille), pm(p.CritAvailPermille), pm(p.PSIWarn10), pm(p.PSICrit10))
	row("budgets", "warn at %s of budget, kill above budget +%s", pm(p.BudgetWarnPermille), pm(p.TolerancePermille))
	if r.Platform != "darwin" {
		row("hard cap", "%s (linux job containers: memory.max = budget +%s in enforce mode)", onoff(r.HardCap), pm(p.TolerancePermille))
	}
	row("enforcement", "warn deadline %s, grace %s (critical %s), step %s (critical %s), recover hold %s, orphan grace %s",
		ms(p.WarnDeadlineMS), ms(p.GraceMS), ms(p.CriticalGraceMS), ms(p.StepMS), ms(p.CriticalStepMS),
		ms(p.RecoverHoldMS), ms(p.OrphanGraceMS))
	row("victims", "process groups >= %s (under pressure also a swarm of smaller unmanaged ones >= %s)",
		by(p.MinVictimBytes), by(p.MinVictimBytes/policy.SmallVictimDivisor))
	ub := "none"
	if p.UnmanagedBudgetBytes > 0 {
		ub = by(p.UnmanagedBudgetBytes)
	}
	scope := "all processes of the user"
	if r.ScopeRootPID > 0 {
		scope = fmt.Sprintf("descendants of pid %d", r.ScopeRootPID)
	}
	row("unmanaged", "priority %d, budget %s, scope %s, notify_stderr %s (linux)", p.UnmanagedPriority, ub, scope, onoff(r.NotifyStderr))
	if len(r.ProtectNames) > 0 {
		row("protected", "never acted on: %s (and their process groups)", strings.Join(r.ProtectNames, ", "))
	}
	row("queue", "timeout %s", ms(p.DefaultQueueTimeoutMS))
	row("cpu", "throttle above load %s/core, lift below %s/core",
		strconv.FormatFloat(float64(p.CPUWarnPerCoreMilli)/1000, 'f', -1, 64),
		strconv.FormatFloat(float64(p.CPUOKPerCoreMilli)/1000, 'f', -1, 64))
	row("disk", "%s: warn below %s free, critical below %s", strings.Join(r.DiskPaths, ", "),
		by(p.DiskWarnFreeBytes), by(p.DiskCritFreeBytes))
	row("learning", "margin %s, history %d, min samples %d, max keys %d", pm(p.MarginPermille), p.HistorySize, p.HistoryMinSamples, p.HistoryMaxKeys)
	row("fail-open", "%d slots, wait %s", r.FailOpenSlots, ms(r.FailOpenWaitMS))
	row("notify", "macos %s", onoff(r.NotifyMacOS))
	tw.Flush()

	fmt.Fprintf(&b, "classes (default %q):\n", p.DefaultClass)
	tw = tabwriter.NewWriter(&b, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tESTIMATE\tBUDGET\tSLOTS\tWALL\tNO-PROGRESS\tPRIORITY")
	lim := func(v int64) string {
		if v == 0 {
			return "none"
		}
		return ms(v)
	}
	for _, c := range p.Classes {
		slots := strconv.Itoa(c.Slots)
		if c.Slots == 0 {
			slots = "unlimited"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%d\n", c.Name, by(c.EstimateBytes), by(c.BudgetBytes),
			slots, lim(c.WallLimitMS), lim(c.NoProgressMS), c.Priority)
	}
	tw.Flush()
	fmt.Fprintf(&b, "hash %s\n", r.Hash)
	return b.String()
}
