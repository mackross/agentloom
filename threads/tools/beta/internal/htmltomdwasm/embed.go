package htmltomdwasm

import _ "embed"

// wasmBytes embeds a pinned wasm-bindgen artifact from
// @kreuzberg/html-to-markdown-wasm.
//
// To update it:
//
//  1. Download the desired npm package version, for example:
//
//     npm pack @kreuzberg/html-to-markdown-wasm@3.4.0-rc.21
//
//  2. Extract package/dist-node/html_to_markdown_wasm_bg.wasm from the tarball
//     and replace internal/htmltomdwasm/html_to_markdown_wasm_bg.wasm.
//
//  3. Check package/dist-node/html_to_markdown_wasm.js for changed exported
//     function names or imported wasm-bindgen glue names. If they changed,
//     update htmltomdwasm.go accordingly.
//
//  4. Run:
//
//     go test ./internal/htmltomdwasm
//     go test ./...
//
// This is intentionally pinned because the npm WASM is wasm-bindgen output, not
// a stable WASI/component ABI.
//
//go:embed html_to_markdown_wasm_bg.wasm
var wasmBytes []byte
