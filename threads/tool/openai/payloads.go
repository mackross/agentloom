// Package openai marks OpenAI-specific tool payload forms.
// RegexpDefinitionString and LarkDefinitionString live here.
// Package threads/tool redefines them for ordinary callers.
package openai

type (
	RegexpDefinitionString string
	LarkDefinitionString   string
)
