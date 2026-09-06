package ollama

import "github.com/mackross/agentloom/llms"

// SetEffort maps a global effort level onto Ollama's think field.
// EffortDefault restores the value the streamer was opened with.
func (s *ChatStreamer) SetEffort(e llms.Effort) error {
	switch e {
	case llms.EffortDefault:
		s.Think = s.defaultThink
	case llms.EffortNone:
		s.Think = false
	case llms.EffortMinimal:
		s.Think = "low"
	case llms.EffortXHigh:
		s.Think = "max"
	default:
		s.Think = string(e)
	}
	return nil
}
