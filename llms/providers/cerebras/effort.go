package cerebras

import "github.com/mackross/agentloom/llms"

// SetEffort maps a global effort level onto the chat-completions
// reasoning_effort field. EffortDefault clears it.
func (s *ChatCompletionsStreamer) SetEffort(e llms.Effort) error {
	s.ReasoningEffort = string(e)
	return nil
}

// SetFast toggles the priority service tier.
func (s *ChatCompletionsStreamer) SetFast(on bool) error {
	s.ServiceTier = ""
	if on {
		s.ServiceTier = "priority"
	}
	return nil
}
