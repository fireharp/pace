package policy

// cloneState deep-copies s so that Step never aliases the caller's slices.
func cloneState(s State) State {
	out := s
	if s.Jobs != nil {
		out.Jobs = make([]Job, len(s.Jobs))
		for i, j := range s.Jobs {
			j.Enf = cloneEnf(j.Enf)
			out.Jobs[i] = j
		}
	}
	if s.Units != nil {
		out.Units = make([]Unit, len(s.Units))
		for i, u := range s.Units {
			u.Enf = cloneEnf(u.Enf)
			out.Units[i] = u
		}
	}
	if s.History != nil {
		out.History = make([]History, len(s.History))
		for i, h := range s.History {
			out.History[i] = History{
				Key:    h.Key,
				Peaks:  append([]int64(nil), h.Peaks...),
				DurMS:  append([]int64(nil), h.DurMS...),
				LastMS: h.LastMS,
			}
		}
	}
	return out
}

func cloneEnf(e Enforcement) Enforcement {
	if e.Warnings != nil {
		e.Warnings = append([]Warning(nil), e.Warnings...)
	}
	return e
}
