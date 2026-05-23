package smartturn

import (
	"context"
	"fmt"
	"time"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

type Detector struct {
	runtime   *ort.Runtime
	env       *ort.Env
	session   *ort.Session
	extractor *FeatureExtractor
}

type DetectorConfig struct {
	LibraryPath string
	ModelPath   string
	APIVersion  uint32
	Threads     int
}

type Result struct {
	Complete    bool
	Probability float32
	Duration    time.Duration
}

func NewDetector(cfg DetectorConfig) (*Detector, error) {
	if cfg.LibraryPath == "" {
		var err error
		cfg.LibraryPath, err = BundledONNXRuntimeLibraryPath()
		if err != nil {
			return nil, err
	}
	if cfg.ModelPath == "" {
		var err error
		cfg.ModelPath, err = BundledSmartTurnModelPath()
		if err != nil {
			return nil, err
		}
	}
	}
	apiVersion := cfg.APIVersion
	if apiVersion == 0 {
		apiVersion = 23
	}
	threads := cfg.Threads
	if threads == 0 {
		threads = 1
	}
	runtime, err := ort.NewRuntime(cfg.LibraryPath, apiVersion)
	if err != nil {
		return nil, err
	}
	d := &Detector{runtime: runtime, extractor: NewFeatureExtractor()}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	d.env, err = runtime.NewEnv("agentloom-smartturn", ort.LoggingLevelWarning)
	if err != nil {
		return nil, err
	}
	d.session, err = runtime.NewSession(d.env, cfg.ModelPath, &ort.SessionOptions{IntraOpNumThreads: threads})
	if err != nil {
		return nil, err
	}
	if got := d.session.InputNames(); len(got) != 1 || got[0] != "input_features" {
		return nil, fmt.Errorf("unexpected Smart Turn inputs: %v", got)
	}
	if got := d.session.OutputNames(); len(got) != 1 || got[0] != "logits" {
		return nil, fmt.Errorf("unexpected Smart Turn outputs: %v", got)
	}
	return d, nil
}

func (d *Detector) Close() {
	if d.session != nil {
		d.session.Close()
		d.session = nil
	}
	if d.env != nil {
		d.env.Close()
		d.env = nil
	}
	if d.runtime != nil {
		_ = d.runtime.Close()
		d.runtime = nil
	}
}

func (d *Detector) PredictPCM16(ctx context.Context, pcm []int16, sampleRate int) (Result, error) {
	if sampleRate != SampleRate {
		return Result{}, fmt.Errorf("smartturn: expected %d Hz audio, got %d", SampleRate, sampleRate)
	}
	audio := make([]float32, len(pcm))
	for i, s := range pcm {
		audio[i] = float32(s) / 32768.0
	}
	return d.PredictFloat32(ctx, audio)
}

func (d *Detector) PredictFloat32(ctx context.Context, audio []float32) (Result, error) {
	features := d.extractor.Extract(audio)
	return d.PredictFeatures(ctx, features)
}

func (d *Detector) PredictFeatures(ctx context.Context, features []float32) (Result, error) {
	if len(features) != FeatureSize*NumFrames {
		return Result{}, fmt.Errorf("smartturn: expected %d features, got %d", FeatureSize*NumFrames, len(features))
	}
	input, err := ort.NewTensorValue(d.runtime, features, []int64{1, FeatureSize, NumFrames})
	if err != nil {
		return Result{}, err
	}
	defer input.Close()
	start := time.Now()
	outputs, err := d.session.Run(ctx, map[string]*ort.Value{"input_features": input}, ort.WithOutputNames("logits"))
	if err != nil {
		return Result{}, err
	}
	out := outputs["logits"]
	defer out.Close()
	data, _, err := ort.GetTensorData[float32](out)
	if err != nil {
		return Result{}, err
	}
	if len(data) != 1 {
		return Result{}, fmt.Errorf("smartturn: expected one output logit/probability, got %d", len(data))
	}
	p := data[0]
	return Result{Complete: p > 0.5, Probability: p, Duration: time.Since(start)}, nil
}
