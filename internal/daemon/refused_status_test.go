package daemon

import (
	"encoding/json"
	"testing"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// A job refused in its first step shows the class it was governed as (an
// unknown --class falls back to the default class) and its estimate.
func TestRefusedJobStatusUsesResolvedClass(t *testing.T) {
	h := newHarness(t, hopt{fn: policy.Step, cfg: func(c *config.Resolved) {
		for i := range c.Params.Classes {
			if c.Params.Classes[i].Name == "default" {
				c.Params.Classes[i].BudgetBytes, c.Params.Classes[i].Priority = 2<<30, 6
			}
		}
	}})
	s := h.openStream("/v1/jobs", proto.SubmitRequest{Class: "bulid", Argv: []string{"x"}, EstimateBytes: 50 << 30, NoWait: true})
	acc := s.expect(proto.VerbAccepted)
	r := s.next()
	for r.Verb != proto.VerbRefused { // the policy's ACCEPTED, INFO unknown class
		r = s.next()
	}
	if r.Class != "default" {
		t.Fatalf("REFUSED class=%q", r.Class)
	}
	var js proto.JobStatus
	eventually(t, "refused status", func() bool {
		_, b := h.get("/v1/jobs/" + acc.Job)
		js = proto.JobStatus{}
		return json.Unmarshal(b, &js) == nil && js.State == policy.JobDone
	})
	if js.Class != "default" || js.EstimateBytes != 50<<30 {
		t.Fatalf("status class=%q estimate=%d, want default, %d", js.Class, js.EstimateBytes, int64(50<<30))
	}
	// Budget and priority as the policy resolved them (its ACCEPTED event):
	// the class budget raised to the declared estimate, the class priority.
	if js.BudgetBytes != 50<<30 || js.Priority != 6 {
		t.Fatalf("status budget=%d priority=%d, want %d, 6", js.BudgetBytes, js.Priority, int64(50<<30))
	}
}
