package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	st "github.com/gonnx-models/smartturn"
	"github.com/mackross/gonnx"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

func main() {
	runs := flag.Int("runs", 10, "number of inference runs")
	threads := flag.Int("threads", 1, "ORT intra-op threads")
	provider := flag.String("provider", "", "optional execution provider to append; empty uses ORT default CPU provider")
	flag.Parse()

	opts := &ort.SessionOptions{IntraOpNumThreads: *threads}
	if *provider != "" {
		opts.ExecutionProviders = []string{*provider}
	}
	session, err := st.OpenSession(gonnx.WithSessionOptions(opts))
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	fmt.Printf("ort version: %s api=%d\n", session.Runtime.GetVersionString(), session.Runtime.GetAPIVersion())
	providers, err := session.Runtime.GetAvailableProviders()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("providers: %v\n", providers)
	fmt.Printf("inputs:  %v\n", session.InputNames())
	fmt.Printf("outputs: %v\n", session.OutputNames())

	features := make([]float32, st.FeatureSize*st.NumFrames)
	input, err := gonnx.Tensor(session.Runtime, features, 1, st.FeatureSize, st.NumFrames)
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
		data, shape, err := gonnx.TensorData[float32](out)
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
