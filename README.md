# agentloom

agentloom is a Go library for building agents for real workloads not just
prototypes.

It provides forkable conversation threads that can reliably recover from
interrupted streams with different recovery policies that provide different
guarantees.

The storage interface is based on a write-ahead log (WAL) with checkpoints,
which aligns with most storage patterns (fast append, slower update).

## Features

- Provider-agnostic request construction from concrete transcript items (switch models mid-conversation).
- Streaming model execution with tool-call chunk materialization.
- Tool execution lifecycle markers for resolving, started, recovered, and completed calls.
- Typed helpers for JSON-schema tools and structured tool results.
- Multitool support for stable provider-visible tool surfaces with non cache busting change of subcommands.
- Safe request projection for model-correctable tool failures.
- Branch storage and leases for durable multi-session workflows.
- Provider adapters for OpenAI, Anthropic, Fireworks, Google Gemini, and Cerebras.

## Packages

- `threads`: conversation state, request construction, streaming execution, tool calls,
  branches, checkpoints, WAL replay, and explicit recovery attach helpers.
- `threads/tool`: helpers for defining catalogs, typed JSON handlers, and tool results.
- `threads/tool/multitool`: one stable model-facing tool that routes command-style calls
  to hidden subtools, with Lark/custom-tool and JSON modes.
- `threads/durability`: local file-backed durable thread storage.
- `threads/durability/sqlitebranchstore`: SQLite branch, lease, checkpoint, and WAL storage.
- `llms`: provider-neutral model catalog, effort levels, provider-defined service
  tiers, and provider descriptions.
- `llms/providers/openai`: OpenAI Responses API streamer with websocket/SSE transports,
  previous-response continuation, function tools, custom grammar tools, and Codex
  subscription sign-in. GPT-6 Astra (`astra`) uses the GPT-6 effort vocabulary
  (`low`..`max`, no `none`/`minimal`), `prompt_cache_options.ttl` caching, and
  mid-conversation `configuration_update` effort changes.
- `llms/providers/anthropic`: Anthropic Messages API streamer.
- `llms/providers/xai`: xAI Responses streamer with Grok subscription sign-in.
- `llms/providers/googlegenai`: Google Gemini generateContent streamer.
- `llms/providers/{fireworks,cerebras,deepseek,ollama}`: further streamers.
- `llms/providers/all`: every built-in provider in one call.
- `llms/subscription`: device sign-in flows and refreshing token sources.
- `llms/cache/*`: provider-specific prompt-cache metadata helpers.
- `harness`: shared configuration files, credentials and sign-in, and a live model
  session for building agent harnesses.

## Install

```sh
go get github.com/mackross/agentloom
```

## Minimal example

```go
thread := threads.New()
thread.SetExecutor(threads.NewThreadExecutor(streamer))

thread.QueueItem(threads.AssistantInstruction("Be concise."))
thread.QueueItem(threads.UserText("Hello"))
thread.QueueItem(threads.SendItem{})
```

The executor builds a provider request from the thread, streams model output back into
history, resolves tool calls when configured, and returns the thread to idle.

## Service tiers

Tier IDs belong to providers; there is no global Standard/Fast/Ultrafast enum.
Every model's `ServiceTiers` lists known tiers in provider preference order
(fastest first), including `ID`, `Label`, `MaxMultiplier`, and availability
`Notes`. The same metadata is available through `h.Models()`, `h.ModelOptions(ctx)`,
and `session.Model()` for UI menus.

```go
// Render each entry's Label, MaxMultiplier, and Notes in your UI.
tiers := session.Model().ServiceTiers

// Select the preferred known tier whose token-rate premium is at most 2×.
// The ceiling is remembered and re-resolved when switching models.
err := session.SetServiceTierForMultiplier(2)

// Or explicitly select a provider-defined ID from the current model's menu.
if len(tiers) > 0 {
    err = session.SetServiceTier(tiers[0].ID)
}

// Reset to provider/project defaults, which may differ from standard pricing.
err = session.SetServiceTier("")
```

For standalone catalog use, `model.ServiceTierForMultiplier(ceiling)` returns an
ID that can be passed to a streamer's `llms.ServiceTierSetter`. Unknown pricing
(`MaxMultiplier == 0`) is never selected automatically. Invalid ceilings and
models with no qualifying known tier return errors rather than silently using
project defaults. A multiplier is a conservative token-rate ratio, not a
total-spend limit.

OpenAI Astra lists Ultrafast (6×), Fast (2×), Standard (1×), and Flex (0.5×).
Other models have their own lists and prices. xAI Priority is 2×, the curated
Google text models list Priority at 1.8×, and Fireworks Priority is model-specific
(1.2×, 1.25×, or 1.5×). Cerebras Shared Inference does not support service tiers,
so its curated models do not advertise dedicated-only Priority.
Catalog capabilities do not guarantee API-key, subscription, regional, or
preview access. Check `Notes` and filter tiers for your deployment as needed.

Preferences are persisted in the shared/app model TOML files:

```toml
[service_tier]
max_multiplier = 2.0
```

An explicit preference instead contains `provider`, `model` (the wire ID), and
`id`. It resets when switching to a different provider/model; a multiplier
preference carries over and is resolved again. An app-level empty
`[service_tier]` table clears an inherited preference. Legacy `fast` settings and
`SetFast(bool)` remain supported; they do not select Ultrafast.

Configured model entries can supply `service_tiers` as an array of tables in
preference order, for example:

```toml
[[models]]
provider = "openai"
name = "astra-eu"
id = "gpt-6-astra"
service_tiers = [
  { id = "default", label = "Standard", max_multiplier = 1.0 },
  { id = "flex", label = "Flex", max_multiplier = 0.5 },
]
```

## Current model catalogs

The catalogs are curated for the adapters we implement, not every modality
advertised by a provider. See [catalog sources and availability](llms/MODEL_CATALOG.md).

- **xAI:** `grok` now selects Grok 4.7, including `xhigh` reasoning. The explicit
  Grok 4.6 ID and `grok-4.6-latest` alias still select 4.6.
- **Google:** Gemini 3.8 Flash remains the default. Pro, Flash-Lite, and other
  current Gemini 3 text models are listed with model-specific thinking levels.
  Live, TTS, image-output, and video models are not added to this text adapter.
- **Cerebras:** Shared Inference lists GPT-OSS 120B (default) and Qwen 3.8 27B
  (`cerebras-qwen`). Gemma 4 is dedicated-only; old Llama 3.1 and Qwen 3 235B entries
  are no longer curated. Qwen does not receive the unsupported `hidden`
  reasoning format.
- **Fireworks:** serverless only. Kimi K3 remains the default; the catalog also
  includes Ember 1, DeepSeek V4.1 Flash, GLM 5.3/Flash, Qwen 3.8 Max, MiniMax M3,
  GPT-OSS 120B, and Nemotron models. Kimi K3 Fast and GLM 5.3 Fast are separate
  router IDs, not `service_tier` values. Known dedicated-only former catalog
  entries are rejected when opened through the provider.

Hosted open-weight model aliases include their provider: `cerebras-qwen`,
`fireworks-qwen`, `fireworks-glm`, `fireworks-kimi`, and `fireworks-minimax`.
Fast variants use `fireworks-kimi-fast` and `fireworks-glm-fast`. Bare aliases
such as `qwen` and `glm` are not claimed by those providers, leaving them
available for configured models (for example, on Ollama). Full model IDs are
unchanged.

## Google Gemini

`llms/providers/googlegenai` defaults an empty model name to the stable
`gemini-3.8-flash` model. The default client reads `GEMINI_API_KEY` or
`GOOGLE_API_KEY`:

```go
streamer := googlegenai.NewGenerateContentStreamer("")
streamer.Config.ThinkingConfig = &genai.ThinkingConfig{
	ThinkingLevel:   genai.ThinkingLevelMedium,
	IncludeThoughts: true, // Optional: emit available reasoning summaries.
}
```

Gemini 3.8 Flash supports `low`, `medium`, and `high` thinking levels, with
`medium` as the service default. It does not support `minimal` thinking or
multiple candidates; the adapter rejects those options before sending a
request. It preserves signed Gemini reasoning across turns for stronger
long-running tool workflows; retained reasoning can increase input token
usage.

## Tooling

Tool execution is designed around durable boundaries. See:

- [`threads/TOOL_HYDRATION.md`](./threads/TOOL_HYDRATION.md)
- [`threads/TOOL_DISPATCH.md`](./threads/TOOL_DISPATCH.md)

Tool results are concrete `threads.ToolCallResult` values:

```go
type ToolCallResult struct {
    CallID       string
    Output       string
    Recovered    bool
    Data         map[string]any
    SafeRollback *ToolCallSafeRollback
}
```

`Output` is the model-visible result. `Data` is caller-owned structured data for UI,
debugging, and application state; agentloom does not use magic `Data` keys for control flow.

## Multitool

`threads/tool/multitool` exposes one stable provider-visible tool and routes calls to
hidden subtools.

This is useful when an application wants to keep the model-facing tool surface stable
while changing the command set behind it, or when a workflow should choose one command
from a set without advertising every command as a separate provider tool.

Multitool supports:

- `ModeLark`: custom grammar tool input shaped as:

  ```text
  command arg...

  input text
  ```

- `ModeJSON`: JSON input with `command` and `input` fields.
- JSON subtools that decode command input into typed Go structs.
- Fallback responses for empty or unknown commands.

Example:

```go
type ticketArgs struct {
    Title    string `json:"title"`
    Priority int    `json:"priority"`
}

mt := multitool.New(multitool.Setup{
    Name: "tool",
    Mode: multitool.ModeLark,
}, multitool.Config{
    Subtools: []multitool.Subtool{
        multitool.JSONHandler[ticketArgs](
            multitool.SubtoolSpec{
                Command:     "create-ticket",
                Description: "Create a ticket.",
                Usage:       `{"title":"Bug","priority":3}`,
            },
            "create_ticket",
            tool.JSONHandler(func(ctx context.Context, t *threads.Thread, call tool.Call, args ticketArgs) tool.Item {
                return tool.ResultText(call, "created")
            }),
        ),
    },
})

thread.SetToolProvider(mt)
thread.SetToolResolver(mt)
```

## Safe tool-call repair

Some tool failures are model-correctable, such as malformed JSON input. A tool result can
opt into safe rollback projection:

```go
threads.ToolCallResult{
    CallID: call.CallID,
    Output: "invalid JSON input: ...",
    SafeRollback: &threads.ToolCallSafeRollback{
        SteeringHint: `<tool_call_hint tool="tool">Call again with valid JSON.</tool_call_hint>`,
    },
}
```

For streamers that support assistant-prefix continuation, the default request builder may
project the failed tool call/result out of the next request and insert the steering hint as
user text at the retry point.

This does not rewrite durable history. Once a retry succeeds, the hint disappears from
future requests. `multitool.JSONHandler` uses this mechanism for JSON parse/schema failures
and includes the subtool description, usage, and JSON schema in the steering hint.

## Provider capabilities

Streamers report whether they support assistant-prefix continuation. The request builder
uses that capability for rollbackable tool failure repair. Independently of provider, the
thread holds a follow-up request until every tool call in the preceding response has a
terminal result.

## Durability and recovery

agentloom supports snapshots, append-only WAL diffs, checkpoint policies, restore, and
explicit executor attach recovery.

Recovery can resume retained request construction and has limited support for interrupted
streams:

- fail closed on retained tool-call stream material
- roll back interrupted stream output and retry
- keep assistant-prefix output when the target streamer supports it
- append recovered status results for unresolved tool calls with `ToolCallRecoveryCancelAll`

See [`threads/DURABILITY.md`](./threads/DURABILITY.md) and
[`threads/EXECUTOR_RECOVERY.md`](./threads/EXECUTOR_RECOVERY.md).

## Choosing models

`llms` describes every provider the same way: an id, credential environment variables,
an optional subscription sign-in, curated models with the effort levels they accept, a
prefix matcher for uncurated ids, and an `Open` function. `llms/providers/all` gathers
the built-in providers into a catalog that resolves names, aliases, and
`provider/model` references:

```go
cat := all.Catalog()
streamer, model, err := cat.Open(ctx, "sonnet", llms.Options{})   // reads ANTHROPIC_API_KEY
```

Effort is one vocabulary (`default`, `none`, `minimal`, `low`, `medium`, `high`,
`xhigh`, `max`). Each streamer that supports it implements `llms.EffortSetter` and maps
the level onto its own knob; `Model.Efforts` lists what a model accepts.

## Building a harness

`harness` adds the pieces every interactive harness needs: shared configuration under
`~/.config/agents` (`models.loom.toml`, a per-app `models.<app>.loom.toml` overlay, and
`auth.loom.toml`), credential resolution, subscription sign-in, and a live session whose
model, effort, and fast settings are remembered.

```go
h, err := harness.Open(ctx, harness.Options{App: "weaver"})
ses, err := h.Session(ctx, "")            // remembered default
exec := threads.NewThreadExecutor(ses.Streamer())

ses.Switch(ctx, "sol")                    // /model sol
ses.SetEffort(llms.EffortHigh)            // /effort high
h.SignIn(ctx, openai.ID, showPrompt)      // device sign-in
```

`models.loom.toml` can add models (with provider-specific keys such as an Ollama host) and
set defaults:

```toml
default = "sol"
effort = "high"

[[models]]
provider = "ollama"
name = "qwen"
id = "qwen3.5:9b-mlx"
host = "http://terminus.local:11434"
stream_progress_timeout = "30m"
efforts = ["none", "low", "high", "max"]
```

For Ollama, `stream_progress_timeout` is a per-model client-side inactivity
timeout. Set it directly in the `[[models]]` entry, not in `[models.options]`.
It accepts a Go duration string: omitted means `"5m"`, `"30m"` allows longer
silent prompt evaluation, and `"0s"` disables the watchdog. Invalid or negative
values are rejected when the model is opened. It starts before the request,
resets on response headers and every body read (including thinking and partial
JSON), and does not limit the total duration of a stream that keeps making
progress. Caller deadlines and HTTP-client timeouts remain independent.

## Provider examples

Interactive examples live in:

- `threads/examples/chat`
- `threads/examples/chat_event_loop`

They open a `harness.Session` for the model named by `MODEL` (or the remembered default)
and switch models with `/model`.

## Live provider tests

Most tests are offline. Live tests are behind the `live` build tag and provider environment
variables.

```sh
OPENAI_API_KEY=... go test -tags live ./llms/providers/openai ./threads
```

For the OpenAI multitool repair test:

```sh
OPENAI_API_KEY=... OPENAI_MULTITOOL_LIVE_MODEL=gpt-5.5 \
  go test -tags live ./threads -run TestLiveMultitoolLarkJSONRepairWithOpenAIResponses -count=1 -v
```

## Development checks

```sh
go test ./...
```

## License

MIT. See [`LICENSE`](./LICENSE).
