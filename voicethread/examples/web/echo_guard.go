package main

import (
	"encoding/json"
	"fmt"

	"github.com/mackross/agentloom/voicethread"
)

type echoGuardPayload struct {
	Sensitivity float64 `json:"sensitivity"`
	WakeRMS     float64 `json:"wake_rms"`
	WakeVAD     float64 `json:"wake_vad"`
}

func defaultEchoGuardSensitivity() float64 {
	return envFloat("LOCAL_TURN_ECHO_GUARD_SENSITIVITY", 0)
}

func echoGuardForSensitivity(sensitivity float64) echoGuardPayload {
	sensitivity = clampTurnSensitivity(sensitivity)
	balancedRMS := envFloat("LOCAL_TURN_BARGE_IN_WAKE_RMS", 0.006)
	balancedVAD := envFloat("LOCAL_TURN_BARGE_IN_WAKE_VAD_THRESHOLD", envFloat("LOCAL_TURN_INTERRUPT_VAD_THRESHOLD", 0.98))

	// -1 = favor fast barge-in, +1 = favor echo safety. Keep the 0 position at
	// the currently working defaults.
	eagerRMS := envFloat("LOCAL_TURN_ECHO_GUARD_EAGER_RMS", 0.0035)
	eagerVAD := envFloat("LOCAL_TURN_ECHO_GUARD_EAGER_VAD", 0.950)
	safeRMS := envFloat("LOCAL_TURN_ECHO_GUARD_SAFE_RMS", 0.012)
	safeVAD := envFloat("LOCAL_TURN_ECHO_GUARD_SAFE_VAD", 0.995)

	var wakeRMS, wakeVAD float64
	if sensitivity < 0 {
		wakeRMS = lerp(eagerRMS, balancedRMS, sensitivity+1)
		wakeVAD = lerp(eagerVAD, balancedVAD, sensitivity+1)
	} else {
		wakeRMS = lerp(balancedRMS, safeRMS, sensitivity)
		wakeVAD = lerp(balancedVAD, safeVAD, sensitivity)
	}
	if wakeRMS < 0 {
		wakeRMS = 0
	}
	if wakeVAD < 0.01 {
		wakeVAD = 0.01
	}
	if wakeVAD > 0.999 {
		wakeVAD = 0.999
	}
	return echoGuardPayload{Sensitivity: sensitivity, WakeRMS: wakeRMS, WakeVAD: wakeVAD}
}

func (b *audioBridge) SetEchoGuardSensitivity(v float64) echoGuardPayload {
	v = clampTurnSensitivity(v)
	b.mu.Lock()
	b.echoGuardSensitivity = v
	b.mu.Unlock()
	return b.EchoGuard()
}

func (b *audioBridge) EchoGuardSensitivity() float64 {
	b.mu.RLock()
	v := b.echoGuardSensitivity
	b.mu.RUnlock()
	return clampTurnSensitivity(v)
}

func (b *audioBridge) EchoGuard() echoGuardPayload {
	return echoGuardForSensitivity(b.EchoGuardSensitivity())
}

func emitEchoGuard(emit func(voicethread.Event), guard echoGuardPayload) {
	raw, _ := json.Marshal(guard)
	emit(voicethread.Event{
		Type:    "local.echo.guard",
		Message: fmt.Sprintf("echo_guard=%.2f wake_rms=%.4f wake_vad=%.3f", guard.Sensitivity, guard.WakeRMS, guard.WakeVAD),
		Raw:     raw,
	})
}
