package htmltomdwasm

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Options controls HtmlToMd conversion behavior.
type Options struct {
	// StripMetaFrontMatter removes the leading metadata block emitted by
	// html-to-markdown when extract_metadata is enabled. The block is recognized
	// as initial lines of "key: value" pairs before the page content.
	StripMetaFrontMatter bool
}

// HtmlToMd converts HTML to Markdown using the pinned Kreuzberg html-to-markdown
// wasm-bindgen artifact embedded in this package. It passes static conversion
// options that skip images and avoid SVG/image extraction.
func HtmlToMd(ctx context.Context, html string) (string, error) {
	return HtmlToMdWithOptions(ctx, html, Options{})
}

// HtmlToMdWithOptions converts HTML to Markdown with caller-controlled output
// post-processing options.
func HtmlToMdWithOptions(ctx context.Context, html string, opts Options) (string, error) {
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV2))
	defer r.Close(ctx)

	if err := instantiateWasmBindgenImports(ctx, r); err != nil {
		return "", err
	}

	mod, err := r.Instantiate(ctx, wasmBytes)
	if err != nil {
		return "", fmt.Errorf("instantiate html-to-markdown wasm: %w", err)
	}

	malloc := mod.ExportedFunction("__wbindgen_export")
	stackPtr := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	optionsDefault := mod.ExportedFunction("wasmconversionoptions_default")
	setSkipImages := mod.ExportedFunction("wasmconversionoptions_set_skipImages")
	setExtractImages := mod.ExportedFunction("wasmconversionoptions_set_extractImages")
	setCaptureSVG := mod.ExportedFunction("wasmconversionoptions_set_captureSvg")
	setMaxImageSize := mod.ExportedFunction("wasmconversionoptions_set_maxImageSize")
	convert := mod.ExportedFunction("convert")
	content := mod.ExportedFunction("wasmconversionresult_content")
	free := mod.ExportedFunction("__wbindgen_export4")
	if malloc == nil || stackPtr == nil || optionsDefault == nil || setSkipImages == nil || setExtractImages == nil || setCaptureSVG == nil || setMaxImageSize == nil || convert == nil || content == nil || free == nil {
		return "", errors.New("html-to-markdown wasm missing expected wasm-bindgen exports")
	}

	retptr, err := callI32(ctx, stackPtr, uint64(^uint32(15)))
	if err != nil {
		return "", fmt.Errorf("allocate result stack: %w", err)
	}
	defer stackPtr.Call(ctx, uint64(16))

	htmlPtr, htmlLen, err := writeString(ctx, mod, malloc, html)
	if err != nil {
		return "", err
	}

	optionsPtr, err := staticOptions(ctx, optionsDefault, setSkipImages, setExtractImages, setCaptureSVG, setMaxImageSize)
	if err != nil {
		return "", err
	}

	if _, err := convert.Call(ctx, uint64(retptr), uint64(htmlPtr), uint64(htmlLen), uint64(optionsPtr), 0); err != nil {
		return "", fmt.Errorf("convert html-to-markdown wasm: %w", err)
	}
	mem := mod.Memory()
	resPtr, ok := mem.ReadUint32Le(retptr)
	if !ok {
		return "", errors.New("read conversion result pointer")
	}
	errRef, ok := mem.ReadUint32Le(retptr + 4)
	if !ok {
		return "", errors.New("read conversion error ref")
	}
	errFlag, ok := mem.ReadUint32Le(retptr + 8)
	if !ok {
		return "", errors.New("read conversion error flag")
	}
	if errFlag != 0 {
		return "", fmt.Errorf("conversion failed: wasm externref %d", errRef)
	}

	contentRetptr, err := callI32(ctx, stackPtr, uint64(^uint32(15)))
	if err != nil {
		return "", fmt.Errorf("allocate content stack: %w", err)
	}
	defer stackPtr.Call(ctx, uint64(16))
	if _, err := content.Call(ctx, uint64(contentRetptr), uint64(resPtr)); err != nil {
		return "", fmt.Errorf("read conversion content: %w", err)
	}
	strPtr, ok := mem.ReadUint32Le(contentRetptr)
	if !ok {
		return "", errors.New("read content pointer")
	}
	strLen, ok := mem.ReadUint32Le(contentRetptr + 4)
	if !ok {
		return "", errors.New("read content length")
	}
	if strPtr == 0 {
		return "", nil
	}
	b, ok := mem.Read(strPtr, strLen)
	if !ok {
		return "", errors.New("read content bytes")
	}
	out := string(append([]byte(nil), b...))
	_, _ = free.Call(ctx, uint64(strPtr), uint64(strLen), 1)
	if opts.StripMetaFrontMatter {
		out = stripMetaFrontMatter(out)
	}
	return out, nil
}

func instantiateWasmBindgenImports(ctx context.Context, r wazero.Runtime) error {
	b := r.NewHostModuleBuilder("./html_to_markdown_wasm_bg.js")
	b = exportStub(b, "__wbg_Error_3639a60ed15f87e7", sig(2), sig(1))
	b = exportStub(b, "__wbg___wbindgen_is_function_2f0fd7ceb86e64c5", sig(1), sig(1))
	b = exportStub(b, "__wbg___wbindgen_is_string_eddc07a3efad52e6", sig(1), sig(1))
	b = exportStub(b, "__wbg___wbindgen_is_undefined_244a92c34d3b6ec0", sig(1), sig(1))
	b = exportStub(b, "__wbg___wbindgen_string_get_965592073e5d848c", sig(2), nil)
	b = exportStub(b, "__wbg___wbindgen_throw_9c75d47bf9e7731e", sig(2), nil)
	b = exportStub(b, "__wbg___wbindgen_is_bigint_55c663b7c0dcba1d", sig(1), sig(1))
	b = exportStub(b, "__wbg_apply_0f21c8b7ff1b23f8", sig(3), sig(1))
	b = exportStub(b, "__wbg_get_41476db20fef99a8", sig(2), sig(1))
	b = exportStub(b, "__wbg_has_3a6f31f647e0ba22", sig(2), sig(1))
	b = exportStub(b, "__wbg_new_2fad8ca02fd00684", nil, sig(1))
	b = exportStub(b, "__wbg_new_3baa8d9866155c79", nil, sig(1))
	b = exportStub(b, "__wbg_new_46ae4e4ff2a07a64", nil, sig(1))
	b = exportStub(b, "__wbg_push_60a5366c0bb22a7d", sig(2), sig(1))
	b = exportStub(b, "__wbg_set_5337f8ac82364a3f", sig(3), sig(1))
	b = exportStub(b, "__wbg_set_6be42768c690e380", sig(3), nil)
	b = exportStub(b, "__wbg_set_82f7a370f604db70", sig(3), sig(1))
	for _, name := range []string{
		"__wbg_wasmdocumentnode_new", "__wbg_wasmdocumentnode_unwrap",
		"__wbg_wasmgridcell_new", "__wbg_wasmgridcell_unwrap",
		"__wbg_wasmheadermetadata_new", "__wbg_wasmheadermetadata_unwrap",
		"__wbg_wasmimagemetadata_new", "__wbg_wasmimagemetadata_unwrap",
		"__wbg_wasmlinkmetadata_new", "__wbg_wasmlinkmetadata_unwrap",
		"__wbg_wasmprocessingwarning_new", "__wbg_wasmprocessingwarning_unwrap",
		"__wbg_wasmstructureddata_new", "__wbg_wasmstructureddata_unwrap",
		"__wbg_wasmtabledata_new", "__wbg_wasmtabledata_unwrap",
		"__wbg_wasmtextannotation_new", "__wbg_wasmtextannotation_unwrap",
		"__wbindgen_object_clone_ref",
	} {
		b = exportStub(b, name, sig(1), sig(1))
	}
	b = exportStub(b, "__wbindgen_cast_0000000000000001", []api.ValueType{api.ValueTypeF64}, sig(1))
	b = exportStub(b, "__wbindgen_cast_0000000000000002", sig(2), sig(1))
	b = exportStub(b, "__wbindgen_object_drop_ref", sig(1), nil)
	_, err := b.Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("instantiate wasm-bindgen imports: %w", err)
	}
	return nil
}

func exportStub(b wazero.HostModuleBuilder, name string, params, results []api.ValueType) wazero.HostModuleBuilder {
	return b.NewFunctionBuilder().WithGoFunction(api.GoFunc(stubI32), params, results).Export(name)
}

func sig(n int) []api.ValueType {
	if n == 0 {
		return nil
	}
	out := make([]api.ValueType, n)
	for i := range out {
		out[i] = api.ValueTypeI32
	}
	return out
}

func stubI32(ctx context.Context, stack []uint64) {
	for i := range stack {
		stack[i] = 0
	}
}

func callI32(ctx context.Context, fn api.Function, args ...uint64) (uint32, error) {
	res, err := fn.Call(ctx, args...)
	if err != nil {
		return 0, err
	}
	return uint32(res[0]), nil
}

func staticOptions(ctx context.Context, optionsDefault, setSkipImages, setExtractImages, setCaptureSVG, setMaxImageSize api.Function) (uint32, error) {
	optionsPtr, err := callI32(ctx, optionsDefault)
	if err != nil {
		return 0, fmt.Errorf("create conversion options: %w", err)
	}
	if _, err := setSkipImages.Call(ctx, uint64(optionsPtr), 1); err != nil {
		return 0, fmt.Errorf("set skip images option: %w", err)
	}
	if _, err := setExtractImages.Call(ctx, uint64(optionsPtr), 0); err != nil {
		return 0, fmt.Errorf("set extract images option: %w", err)
	}
	if _, err := setCaptureSVG.Call(ctx, uint64(optionsPtr), 0); err != nil {
		return 0, fmt.Errorf("set capture svg option: %w", err)
	}
	if _, err := setMaxImageSize.Call(ctx, uint64(optionsPtr), 0); err != nil {
		return 0, fmt.Errorf("set max image size option: %w", err)
	}
	return optionsPtr, nil
}

func writeString(ctx context.Context, mod api.Module, malloc api.Function, s string) (uint32, uint32, error) {
	b := []byte(s)
	ptr, err := callI32(ctx, malloc, uint64(len(b)), 1)
	if err != nil {
		return 0, 0, fmt.Errorf("malloc string: %w", err)
	}
	if !mod.Memory().Write(ptr, b) {
		return 0, 0, errors.New("write string to wasm memory")
	}
	return ptr, uint32(len(b)), nil
}
