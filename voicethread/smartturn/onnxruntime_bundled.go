package smartturn

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

//go:embed onnxruntime/linux-amd64/lib/libonnxruntime.so
//go:embed onnxruntime/linux-amd64/lib/libonnxruntime_providers_shared.so
//go:embed onnxruntime/darwin-arm64/lib/libonnxruntime.dylib
//go:embed models/smart-turn-v3.2-cpu.onnx
//go:embed models/silero_vad.onnx
var bundledONNXRuntime embed.FS

// BundledONNXRuntimeLibraryPath extracts the vendored ONNX Runtime shared
// library for the current platform to a stable temp directory and returns its
// path. It currently supports linux/amd64 and darwin/arm64.
func BundledONNXRuntimeLibraryPath() (string, error) {
	var libraryRel string
	var extraRels []string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		libraryRel = "onnxruntime/linux-amd64/lib/libonnxruntime.so"
		extraRels = []string{"onnxruntime/linux-amd64/lib/libonnxruntime_providers_shared.so"}
	case "darwin/arm64":
		libraryRel = "onnxruntime/darwin-arm64/lib/libonnxruntime.dylib"
	default:
		return "", fmt.Errorf("bundled ONNX Runtime is not available for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	dir := filepath.Join(os.TempDir(), "agentloom-onnxruntime", runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, rel := range append([]string{libraryRel}, extraRels...) {
		if err := extractBundledONNXRuntimeFile(dir, rel); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, filepath.Base(libraryRel)), nil
}

func BundledSmartTurnModelPath() (string, error) {
	return extractBundledModel("models/smart-turn-v3.2-cpu.onnx")
}

func BundledSileroVADModelPath() (string, error) {
	return extractBundledModel("models/silero_vad.onnx")
}

func extractBundledModel(rel string) (string, error) {
	dir := filepath.Join(os.TempDir(), "agentloom-smartturn-models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := extractBundledONNXRuntimeFile(dir, rel); err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(rel)), nil
}

func extractBundledONNXRuntimeFile(dir, rel string) error {
	data, err := bundledONNXRuntime.ReadFile(rel)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.Base(rel))
	sum := sha256.Sum256(data)
	sumPath := dst + ".sha256"
	if got, err := os.ReadFile(sumPath); err == nil && string(got) == hex.EncodeToString(sum[:]) {
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		}
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return err
	}
	return os.WriteFile(sumPath, []byte(hex.EncodeToString(sum[:])), 0o644)
}
