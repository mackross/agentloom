// Package all lists every provider agentloom ships.
package all

import (
	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/llms/providers/anthropic"
	"github.com/mackross/agentloom/llms/providers/cerebras"
	"github.com/mackross/agentloom/llms/providers/deepseek"
	"github.com/mackross/agentloom/llms/providers/fireworks"
	"github.com/mackross/agentloom/llms/providers/googlegenai"
	"github.com/mackross/agentloom/llms/providers/ollama"
	"github.com/mackross/agentloom/llms/providers/openai"
	"github.com/mackross/agentloom/llms/providers/xai"
)

// Providers returns every built-in provider in display order.
func Providers() []*llms.Provider {
	return []*llms.Provider{
		openai.Provider,
		anthropic.Provider,
		googlegenai.Provider,
		xai.Provider,
		cerebras.Provider,
		fireworks.Provider,
		deepseek.Provider,
		ollama.Provider,
	}
}

// Catalog returns a catalog over Providers.
func Catalog() *llms.Catalog {
	return llms.NewCatalog(Providers()...)
}
