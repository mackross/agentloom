// Package fileprocess defines the processor protocol shared by AgentLoom file tools.
//
// The package is intentionally small and neutral: concrete tools such as write,
// apply_patch, and edit construct Requests, applications provide Processors, and
// Run executes a deterministic content pipeline. Higher-level packages may alias
// these types to keep their public API focused.
package fileprocess
