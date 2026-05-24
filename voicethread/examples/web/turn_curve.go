package main

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/mackross/agentloom/voicethread"
)

type turnCurvePayload struct {
	Sensitivity float64              `json:"sensitivity"`
	Thresholds  []turnCurveThreshold `json:"thresholds"`
}

type turnCurveThreshold struct {
	MS        int     `json:"ms"`
	Threshold float64 `json:"threshold"`
}

type turnCurveAnchor struct {
	silenceMSEnv string
	thresholdEnv string
	defaultMS    int
	patient      float64
	balanced     float64
	eager        float64
}

var smartTurnCurveAnchors = []turnCurveAnchor{
	{silenceMSEnv: "SMART_TURN_CHECK_1_MS", thresholdEnv: "SMART_TURN_CHECK_1_THRESHOLD", defaultMS: 250, patient: 0.990, balanced: 0.9825, eager: 0.930},
	{silenceMSEnv: "SMART_TURN_CHECK_2_MS", thresholdEnv: "SMART_TURN_CHECK_2_THRESHOLD", defaultMS: 500, patient: 0.985, balanced: 0.970, eager: 0.820},
	{silenceMSEnv: "SMART_TURN_CHECK_3_MS", thresholdEnv: "SMART_TURN_CHECK_3_THRESHOLD", defaultMS: 800, patient: 0.950, balanced: 0.900, eager: 0.650},
	{silenceMSEnv: "SMART_TURN_CHECK_4_MS", thresholdEnv: "SMART_TURN_CHECK_4_THRESHOLD", defaultMS: 1000, patient: 0.920, balanced: 0.850, eager: 0.600},
	{silenceMSEnv: "SMART_TURN_CHECK_5_MS", thresholdEnv: "SMART_TURN_CHECK_5_THRESHOLD", defaultMS: 2000, patient: 0.980, balanced: 0.940, eager: 0.760},
	// Deliberately humped; balanced remains exactly the fixed threshold from the
	// committed baseline, while patient/eager give the long fallback different
	// behavior without touching VAD/echo handling.
	{silenceMSEnv: "SMART_TURN_CHECK_6_MS", thresholdEnv: "SMART_TURN_CHECK_6_THRESHOLD", defaultMS: 3000, patient: 0.800, balanced: 0.920, eager: 0.700},
	{silenceMSEnv: "SMART_TURN_CHECK_7_MS", thresholdEnv: "SMART_TURN_CHECK_7_THRESHOLD", defaultMS: 4500, patient: 0.900, balanced: 0.900, eager: 0.700},
}

func defaultTurnSensitivity() float64 {
	return envFloat("SMART_TURN_SENSITIVITY", 0)
}

func clampTurnSensitivity(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < -1 {
		return -1
	}
	if v > 1 {
		return 1
	}
	return v
}

func smartTurnThresholdForSensitivity(a turnCurveAnchor, sensitivity float64) float64 {
	sensitivity = clampTurnSensitivity(sensitivity)
	balanced := envFloat(a.thresholdEnv, a.balanced)
	var out float64
	if sensitivity < 0 {
		out = lerp(a.patient, balanced, sensitivity+1)
	} else {
		out = lerp(balanced, a.eager, sensitivity)
	}
	if out < 0.01 {
		return 0.01
	}
	if out > 0.999 {
		return 0.999
	}
	return out
}

func smartTurnCurveForSensitivity(sensitivity float64) turnCurvePayload {
	sensitivity = clampTurnSensitivity(sensitivity)
	thresholds := make([]turnCurveThreshold, 0, len(smartTurnCurveAnchors))
	for _, a := range smartTurnCurveAnchors {
		thresholds = append(thresholds, turnCurveThreshold{
			MS:        envInt(a.silenceMSEnv, a.defaultMS),
			Threshold: smartTurnThresholdForSensitivity(a, sensitivity),
		})
	}
	return turnCurvePayload{Sensitivity: sensitivity, Thresholds: thresholds}
}

func smartTurnCheckpointsForSensitivity(sensitivity float64) []turnCheckpoint {
	curve := smartTurnCurveForSensitivity(sensitivity)
	checkpoints := make([]turnCheckpoint, 0, len(curve.Thresholds))
	for _, p := range curve.Thresholds {
		checkpoints = append(checkpoints, turnCheckpoint{silenceMS: p.MS, threshold: p.Threshold})
	}
	return checkpoints
}

func lerp(a, b, t float64) float64 {
	return a + (b-a)*t
}

func (b *audioBridge) SetTurnSensitivity(v float64) turnCurvePayload {
	v = clampTurnSensitivity(v)
	b.mu.Lock()
	b.turnSensitivity = v
	b.mu.Unlock()
	return b.TurnCurve()
}

func (b *audioBridge) TurnSensitivity() float64 {
	b.mu.RLock()
	v := b.turnSensitivity
	b.mu.RUnlock()
	return clampTurnSensitivity(v)
}

func (b *audioBridge) TurnCurve() turnCurvePayload {
	return smartTurnCurveForSensitivity(b.TurnSensitivity())
}

func (b *audioBridge) TurnCheckpoints() []turnCheckpoint {
	return smartTurnCheckpointsForSensitivity(b.TurnSensitivity())
}

func emitTurnCurve(emit func(voicethread.Event), curve turnCurvePayload) {
	raw, _ := json.Marshal(curve)
	emit(voicethread.Event{
		Type:    "local.turn.curve",
		Message: fmt.Sprintf("turn_sensitivity=%.2f", curve.Sensitivity),
		Raw:     raw,
	})
}
