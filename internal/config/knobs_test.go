package config

import (
	"testing"

	"github.com/fireharp/pace/internal/policy"
)

func TestAdmissionAndLearningKnobs(t *testing.T) {
	const total = 36 * G
	eq := func(name string, got, want any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
	r := mustResolve(t, mustParse(t, ""), total, "darwin", baseEnv())
	eq("admit_below_warn default", r.Params.AdmitBelowWarn, false)
	eq("max_keys default", r.Params.HistoryMaxKeys, policy.DefaultHistoryMaxKeys)
	// The 15% warn line (5.4G) is above the "2G,12.5%" reserve (4.5G).
	eq("effective reserve", policy.EffectiveReserve(r.Params, total), int64(total*150/1000))

	r = mustResolve(t, mustParse(t, "admit_below_warn = true\n[learn]\nmax_keys = 7"), total, "darwin", baseEnv())
	eq("admit_below_warn", r.Params.AdmitBelowWarn, true)
	eq("max_keys", r.Params.HistoryMaxKeys, 7)
	eq("effective reserve below warn", policy.EffectiveReserve(r.Params, total), r.Params.ReserveBytes)
}
