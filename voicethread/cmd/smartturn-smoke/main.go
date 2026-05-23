package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/gomlx/gomlx/backends"
	"github.com/gomlx/gomlx/pkg/core/graph"
	"github.com/gomlx/gomlx/pkg/core/tensors"
	gomlxcontext "github.com/gomlx/gomlx/pkg/ml/context"
	"github.com/gomlx/onnx-gomlx/onnx/parser"
)

func main() {
	modelPath := flag.String("model", "voicethread/models/smart-turn-v3/smart-turn-v3.2-gpu.onnx", "Smart Turn ONNX model path")
	backendConfig := flag.String("backend", "go", "GoMLX backend config")
	runs := flag.Int("runs", 1, "number of inference runs")
	flag.Parse()

	model, err := parser.ParseFile(*modelPath)
	if err != nil {
		log.Fatal(err)
	}
	defer model.Close()

	inputNames, inputShapes := model.Inputs()
	outputNames, outputShapes := model.Outputs()
	fmt.Printf("inputs:  %v %v\n", inputNames, inputShapes)
	fmt.Printf("outputs: %v %v\n", outputNames, outputShapes)
	if len(inputNames) != 1 || inputNames[0] != "input_features" {
		log.Fatalf("unexpected inputs: %v", inputNames)
	}
	if len(outputNames) != 1 || outputNames[0] != "logits" {
		log.Fatalf("unexpected outputs: %v", outputNames)
	}

	ctx := gomlxcontext.New().Reuse()
	defer ctx.Finalize()
	if err := model.VariablesToContext(ctx); err != nil {
		log.Fatal(err)
	}

	backend, err := backends.NewWithConfig(*backendConfig)
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Finalize()

	call := func(ctx *gomlxcontext.Context, inputs []*graph.Node) []*graph.Node {
		return model.CallGraph(ctx, inputs[0].Graph(), map[string]*graph.Node{
			"input_features": inputs[0],
		}, "logits")
	}
	exec, err := gomlxcontext.NewExec(backend, ctx, call)
	if err != nil {
		log.Fatal(err)
	}
	defer exec.Finalize()

	// Smart Turn's ONNX graph expects Whisper log-mel features:
	//   float32[batch, 80, 800]
	// This smoke test uses all-zero features just to prove native-Go model execution.
	features := make([]float32, 1*80*800)
	input := tensors.FromFlatDataAndDimensions(features, 1, 80, 800)
	defer input.FinalizeAll()

	var total time.Duration
	var logits []float32
	for i := range *runs {
		start := time.Now()
		outputs, err := exec.Exec([]*tensors.Tensor{input})
		if err != nil {
			log.Fatal(err)
		}
		outputs[0].MaterializeLocal()

		logits = logits[:0]
		if err := tensors.ConstFlatData(outputs[0], func(flat []float32) {
			logits = append(logits, flat...)
		}); err != nil {
			log.Fatal(err)
		}
		if err := outputs[0].FinalizeAll(); err != nil {
			log.Fatal(err)
		}
		duration := time.Since(start)
		total += duration
		fmt.Printf("run %d: %s logits=%v\n", i+1, duration, logits)
	}
	fmt.Printf("average: %s\n", total/time.Duration(*runs))
}
