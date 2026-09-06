package llms

import (
	"fmt"
	"strings"
)

// Effort is a provider-neutral reasoning effort level. Each model maps the
// global vocabulary onto its own knob and may accept only a subset; see
// Model.Efforts. EffortDefault is always accepted and means "the model's own
// default" (for Anthropic that is adaptive thinking).
type Effort string

const (
	EffortDefault Effort = ""
	EffortNone    Effort = "none"
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

// Efforts lists every explicit level in ascending order. EffortDefault is not
// included because it is always valid.
var Efforts = []Effort{EffortNone, EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// ParseEffort parses a user-supplied level. "" and "default" yield
// EffortDefault. Matching is case-insensitive.
func ParseEffort(s string) (Effort, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "default" {
		return EffortDefault, nil
	}
	for _, e := range Efforts {
		if string(e) == s {
			return e, nil
		}
	}
	return EffortDefault, fmt.Errorf("llms: unknown effort %q; use default, %s", s, joinEfforts(Efforts))
}

func (e Effort) String() string {
	if e == EffortDefault {
		return "default"
	}
	return string(e)
}

func joinEfforts(efforts []Effort) string {
	parts := make([]string, len(efforts))
	for i, e := range efforts {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}
