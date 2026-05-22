// Package voicethread contains a pragmatic OpenAI Realtime voice-agent spike.
//
// The package is intentionally OpenAI-specific and keeps Realtime events mostly
// stringly typed while the API flow, browser audio path, and tool-call loop are
// being explored. It reuses Agentloom's tool snapshot/call/result shapes, but it
// does not use threads.Thread or Agentloom durability yet.
package voicethread
