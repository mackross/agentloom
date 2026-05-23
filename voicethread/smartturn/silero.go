package smartturn

import (
	"context"
	"fmt"
	"time"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

const SileroChunkSamples = 512

type SileroVAD struct {
	runtime *ort.Runtime
	env     *ort.Env
	session *ort.Session
	state   []float32
	context []float32
}

type SileroVADConfig struct {
	LibraryPath string
	ModelPath   string
	APIVersion  uint32
	Threads     int
}

func NewSileroVAD(cfg SileroVADConfig) (*SileroVAD, error) {
	if cfg.LibraryPath == "" {
		var err error
		cfg.LibraryPath, err = BundledONNXRuntimeLibraryPath()
		if err != nil {
			return nil, err
		}
	}
	if cfg.ModelPath == "" {
		var err error
		cfg.ModelPath, err = BundledSileroVADModelPath()
		if err != nil {
			return nil, err
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
	v := &SileroVAD{runtime: runtime, state: make([]float32, 2*1*128), context: make([]float32, 64)}
	defer func() {
		if err != nil {
			v.Close()
		}
	}()
	v.env, err = runtime.NewEnv("agentloom-silero-vad", ort.LoggingLevelWarning)
	if err != nil {
		return nil, err
	}
	v.session, err = runtime.NewSession(v.env, cfg.ModelPath, &ort.SessionOptions{IntraOpNumThreads: threads})
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (v *SileroVAD) Close() {
	if v.session != nil {
		v.session.Close()
		v.session = nil
	}
	if v.env != nil {
		v.env.Close()
		v.env = nil
	}
	if v.runtime != nil {
		_ = v.runtime.Close()
		v.runtime = nil
	}
}

func (v *SileroVAD) Reset() {
	clear(v.state)
	clear(v.context)
}

func (v *SileroVAD) ProbabilityPCM16(ctx context.Context, chunk []int16, sampleRate int) (float32, time.Duration, error) {
	if sampleRate != SampleRate {
		return 0, 0, fmt.Errorf("silero: expected %d Hz, got %d", SampleRate, sampleRate)
	}
	if len(chunk) != SileroChunkSamples {
		return 0, 0, fmt.Errorf("silero: expected %d samples, got %d", SileroChunkSamples, len(chunk))
	}
	inputData := make([]float32, 64+SileroChunkSamples)
	copy(inputData, v.context)
	for i, s := range chunk {
		inputData[64+i] = float32(s) / 32768.0
	}
	input, err := ort.NewTensorValue(v.runtime, inputData, []int64{1, int64(len(inputData))})
	if err != nil {
		return 0, 0, err
	}
	defer input.Close()
	state, err := ort.NewTensorValue(v.runtime, v.state, []int64{2, 1, 128})
	if err != nil {
		return 0, 0, err
	}
	defer state.Close()
	sr, err := ort.NewTensorValue(v.runtime, []int64{int64(sampleRate)}, []int64{})
	if err != nil {
		return 0, 0, err
	}
	defer sr.Close()
	start := time.Now()
	outputs, err := v.session.Run(ctx, map[string]*ort.Value{"input": input, "state": state, "sr": sr})
	if err != nil {
		return 0, 0, err
	}
	dur := time.Since(start)
	out := outputs["output"]
	stateOut := outputs["stateN"]
	defer out.Close()
	defer stateOut.Close()
	prob, _, err := ort.GetTensorData[float32](out)
	if err != nil {
		return 0, 0, err
	}
	newState, _, err := ort.GetTensorData[float32](stateOut)
	if err != nil {
		return 0, 0, err
	}
	copy(v.state, newState)
	copy(v.context, inputData[len(inputData)-64:])
	if len(prob) == 0 {
		return 0, 0, fmt.Errorf("silero: empty output")
	}
	return prob[0], dur, nil
}
