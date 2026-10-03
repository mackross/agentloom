# Model catalog sources and availability

These are curated API model selections, not guarantees of account access.
Explicit model IDs retain their meaning; a removed model is not silently
redirected to a different model. Hosted open-weight aliases are provider-scoped:
`cerebras-qwen`, `fireworks-qwen`, `fireworks-glm`, `fireworks-kimi`, and
`fireworks-minimax`. They may follow a newer model within that provider, never
another provider. No bare `qwen`, `glm`, `kimi`, `ember`, or `minimax` alias is
reserved by these catalogs; local configurations can use those names.

## xAI

Source: [Grok 4.7](https://docs.x.ai/developers/grok-4-7),
[Priority pricing](https://docs.x.ai/developers/pricing#priority-processing-pricing).

- `grok-4.7` is the default (`grok`, `grok-4.7-latest`).
- Efforts: low, medium, high, xhigh. Older Grok effort metadata is unchanged.
- `grok-4.6` remains separately selectable, including `grok-4.6-latest`.
- Priority uses `service_tier: "priority"` at 2x token rates; explicit Standard
  uses `"default"`. The Cursor/Grok Build-only Fast variant is not an API model.

## Google

Sources: [Models](https://ai.google.dev/gemini-api/docs/models),
[Thinking levels](https://ai.google.dev/gemini-api/docs/generate-content/thinking),
[Pricing](https://ai.google.dev/gemini-api/docs/pricing),
[GenerateContent Priority](https://ai.google.dev/gemini-api/docs/generate-content/priority-inference).

| Text model ID | Explicit thinking levels |
| --- | --- |
| `gemini-3.8-flash` (default) | low, medium, high |
| `gemini-3.7-flash` | low, medium, high |
| `gemini-3.6-flash` | minimal, low, medium, high |
| `gemini-3.5-flash` | minimal, low, medium, high |
| `gemini-3.5-flash-lite` | minimal, low, medium, high |
| `gemini-3.1-flash-lite` | minimal, low, medium, high |
| `gemini-3.1-pro-preview` | low, medium, high |
| `gemini-3.1-pro-preview-customtools` | low, medium, high |
| `gemini-3-flash-preview` | minimal, low, medium, high |

These models list Standard and Priority (maximum token-rate ratio 1.8x).
GenerateContent Priority requires billing Tier 2 or 3 and may be downgraded
by the provider to Standard. Flex is not yet listed by this adapter: its cached
token ratios differ across models and cannot all be represented as 0.5x.

The adapter produces text/tool calls, so the catalog excludes Live, TTS, image
generation and video generation models. A broad fallback match must not
assign these models the capabilities of a similarly named text model.

## Cerebras Shared Inference

Sources: [Current shared models](https://inference-docs.cerebras.ai/models/overview),
[Deprecations](https://inference-docs.cerebras.ai/support/deprecation),
[Reasoning](https://inference-docs.cerebras.ai/capabilities/reasoning),
[Service tiers](https://inference-docs.cerebras.ai/capabilities/service-tiers).

- `gpt-oss-120b` (default): low, medium, high reasoning; supports hidden reasoning.
- `qwen-3.8-27b`: none, low, medium, high reasoning; hidden format is unsupported.
- `gemma-4-31b` is dedicated-only, not a shared model.
- `llama3.1-8b` and `qwen-3-235b-a22b-instruct-2507` are deprecated.
- No shared model advertises Priority. The dedicated service-tier preview is
  billed equally across tiers, but this does not make it available on Shared
  Inference. Dedicated deployments require explicit configuration.

The old exported model constants remain available for existing callers, but
they are not recommendations or entries in the shared catalog.

## Fireworks Serverless

Sources: [Serverless pricing](https://docs.fireworks.ai/serverless/pricing),
[Serverless modes](https://docs.fireworks.ai/serverless/serverless-modes),
[Current model aliases](https://docs.fireworks.ai/nexus/open-models).

Model suffixes below have the prefix `accounts/fireworks/models/`.

| Model suffix | Priority / Standard token-rate ratio |
| --- | ---: |
| `kimi-k3` (default) | 1.25 |
| `ember-1` | 1.25 |
| `deepseek-v4p1-flash` | 1.25 |
| `glm-5p3` | 1.25 |
| `glm-5p3-flash` | 1.25 |
| `qwen3p8-max` | 1.5 |
| `minimax-m3` | 1.5 |
| `gpt-oss-120b` | 1.2 |
| `nemotron-lightning-3p5-30b-a3b` | 1.25 |
| `nemotron-3-ultra-nvfp4` (preview) | 1.25 |

Priority uses `service_tier: "priority"`; Standard omits that parameter.

Two Fast router IDs are separately selectable:

- `accounts/fireworks/routers/kimi-k3-fast`
- `accounts/fireworks/routers/glm-5p3-fast`

Their base token rates are already 1.5x their corresponding ordinary models.
Their catalog Standard multiplier is 1x relative to **their own** base prices;
neither offers Priority. Selecting a higher multiplier on the ordinary model
does not automatically switch to a router.

The old `deepseek-v4-pro`, `deepseek-v4-flash-0731`, and `minimax-m2p7` model
pages now list **Serverless not supported**. They are removed from the curated
catalog and rejected by Provider.Open, as are dedicated deployment paths.
Their old exported constants remain for source compatibility, not as supported
serverless selections. Future explicitly named serverless IDs are still allowed,
but receive no guessed pricing metadata.

Regional endpoints, dedicated deployments, and asynchronous Batch jobs are not
curated here.
