package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/mackross/agentloom/voicethread/smartturn"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

func main() {
	libPath := flag.String("lib", "", "path to libonnxruntime; empty extracts bundled library to temp")
	modelPath := flag.String("model", "voicethread/models/smart-turn-v3/smart-turn-v3.2-cpu.onnx", "Smart Turn ONNX model path")
	runs := flag.Int("runs", 10, "number of inference runs")
	threads := flag.Int("threads", 1, "ORT intra-op threads")
	provider := flag.String("provider", "", "optional execution provider to append; empty uses ORT default CPU provider")
	flag.Parse()
	if *libPath == "" {
		var err error
		*libPath, err = smartturn.BundledONNXRuntimeLibraryPath()
		if err != nil {
			log.Fatal(err)
		}
	}

	runtime, err := ort.NewRuntime(*libPath, 23)
	if err != nil {
		log.Fatal(err)
	}
	defer runtime.Close()

	fmt.Printf("ort version: %s api=%d\n", runtime.GetVersionString(), runtime.GetAPIVersion())
	providers, err := runtime.GetAvailableProviders()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("providers: %v\n", providers)

	env, err := runtime.NewEnv("smartturn-ort-smoke", ort.LoggingLevelWarning)
	if err != nil {
		log.Fatal(err)
	}
	defer env.Close()

	opts := &ort.SessionOptions{IntraOpNumThreads: *threads}
	if *provider != "" {
		opts.ExecutionProviders = []string{*provider}
	}
	session, err := runtime.NewSession(env, *modelPath, opts)
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	fmt.Printf("inputs:  %v\n", session.InputNames())
	fmt.Printf("outputs: %v\n", session.OutputNames())

	features := make([]float32, 1*80*800)
	input, err := ort.NewTensorValue(runtime, features, []int64{1, 80, 800})
	if err != nil {
		log.Fatal(err)
	}
	defer input.Close()

	var total time.Duration
	for i := range *runs {
		start := time.Now()
		outputs, err := session.Run(context.Background(), map[string]*ort.Value{
			"input_features": input,
		}, ort.WithOutputNames("logits"))
		if err != nil {
			log.Fatal(err)
		}
		out := outputs["logits"]
		data, shape, err := ort.GetTensorData[float32](out)
		out.Close()
		if err != nil {
			log.Fatal(err)
		}
		duration := time.Since(start)
		total += duration
		fmt.Printf("run %d: %s shape=%v logits=%v\n", i+1, duration, shape, data)
	}
	fmt.Printf("average: %s\n", total/time.Duration(*runs))
}
