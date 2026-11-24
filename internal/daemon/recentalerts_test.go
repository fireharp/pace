package daemon

import (
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

func TestRecentAlerts(t *testing.T) {
	d := &daemon{}
	now := int64(10_000_000)
	d.noteAlert(proto.Event{Verb: proto.VerbAdmitted, TS: now, Job: "j1"})                          // not an alert
	d.noteAlert(proto.Event{Verb: proto.VerbInfo, TS: now, Msg: "daemon started"})                  // INFO without a policy
	d.noteAlert(proto.Event{Verb: proto.VerbWarn, TS: now - recentAlertWindowMS - 1, Job: "old"})   // too old
	d.noteAlert(proto.Event{Verb: proto.VerbWarn, TS: now - 1000, Job: "j2", Policy: "mem_budget"}) // kept
	d.noteAlert(proto.Event{Verb: proto.VerbInfo, TS: now, Job: "j2", Policy: "mem_budget", Mode: "observe", Msg: "would kill"})
	d.noteAlert(proto.Event{Verb: proto.VerbKilled, TS: now, Job: "j3", Policy: "pressure"})
	got := d.recentAlertsAt(now)
	if len(got) != 3 || got[0].Job != "j2" || got[1].Msg != "would kill" || got[2].Verb != proto.VerbKilled {
		t.Fatalf("got %+v", got)
	}
	for i := 0; i < 100; i++ {
		d.noteAlert(proto.Event{Verb: proto.VerbThrottled, TS: now, Job: "x"})
	}
	if n := len(d.recentAlerts); n != maxRecentAlerts {
		t.Fatalf("kept %d, want %d", n, maxRecentAlerts)
	}
	if got := d.recentAlertsAt(now + recentAlertWindowMS + 1); len(got) != 0 {
		t.Fatalf("expired alerts returned: %d", len(got))
	}
}
