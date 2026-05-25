// Package tool builds catalogs and handlers for thread tools.
// It wraps specs, payloads, snapshots, calls, and result items.
// It includes JSON, text, regexp, and Lark payload helpers.
//
// TODO: Decide whether Handler/ReturnItem should accept any Item or only
// threads.ToolCallResult values. ToolDispatch supports arbitrary Items, but a
// normal tool handler must eventually produce a call result to complete the
// pending ToolCall; a result-only API may be simpler and safer.
//
// TODO: Make Catalog a complete easy-path tool surface by adding a provider /
// resolver adapter, so applications can install one catalog without writing
// separate ToolsSnapshot and ResolveTool plumbing.
//
// TODO: Define Catalog hydration semantics for durable HandlerLoadData. The
// catalog should be able to bind specs to stable handler IDs/load data and
// rehydrate handlers after restore instead of relying only on live in-memory
// closures.
//
// TODO: Provide first-class runtime wiring for late returns. Catalog.Dispatch
// currently needs a threads.Thread so asynchronous ReturnItem calls can re-enter
// through the thread's EventLoop; the final helper should make this ergonomic
// without hiding the requirement that late returns need an EventLoop.
//
// TODO: Clarify and test multi-item handling. Immediate returns can currently
// append multiple Items to ToolDispatch.Items; decide whether late returns may
// emit multiple items and how completion/auto-send should behave.
//
// TODO: Add an ergonomic async helper for handlers, including cancellation and
// error-to-result conventions, so tool authors can safely spawn goroutines that
// call ReturnItem exactly once.
//
// TODO: Add examples showing the intended catalog style for sync tools, async
// tools, JSON payload tools, and catalog installation on a Thread/EventLoop.
package tool
