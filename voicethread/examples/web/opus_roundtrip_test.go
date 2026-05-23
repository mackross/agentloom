package main

import (
	"math"
	"os"
	"testing"

	kopus "github.com/kazzmir/opus-go/opus"
)

func TestOpusEncodeDecodeSimilarity(t *testing.T) {
	if os.Getenv("VOICE_OPUS_SIM_TEST") == "" {
		t.Skip("set VOICE_OPUS_SIM_TEST=1 to run the experimental opus similarity check")
	}
	pcm24 := syntheticSpeechLikePCM24(24000 * 2)
	pcm48 := resample24To48(pcm24)
	stereo48 := monoToStereo(pcm48)

	enc, err := kopus.NewEncoder(48000, 2, kopus.ApplicationAudio)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	_ = enc.SetBitrate(64000)
	_ = enc.SetVBR(true)
	_ = enc.SetComplexity(10)

	dec, err := kopus.NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}

	var decodedMono []int16
	packet := make([]byte, 1500)
	for off := 0; off+960*2 <= len(stereo48); off += 960 * 2 {
		n, err := enc.Encode(stereo48[off:off+960*2], 960, packet)
		if err != nil {
			t.Fatalf("encode frame at %d: %v", off, err)
		}
		pcmOut := make([]int16, 960*2)
		got, err := dec.Decode(packet[:n], pcmOut, 960, false)
		if err != nil {
			t.Fatalf("decode frame at %d: %v", off, err)
		}
		for i := 0; i+1 < got*2; i += 2 {
			decodedMono = append(decodedMono, int16((int(pcmOut[i])+int(pcmOut[i+1]))/2))
		}
	}

	corr, lag := bestCorrelation(pcm48, decodedMono, 1200)
	snr := alignedSNRDB(pcm48, decodedMono, lag)
	t.Logf("opus roundtrip similarity: corr=%.4f lag=%d samples snr=%.2f dB", corr, lag, snr)

	if corr < 0.80 {
		t.Fatalf("opus roundtrip correlation too low: %.4f lag=%d snr=%.2f dB", corr, lag, snr)
	}
	if snr < 3.0 {
		t.Fatalf("opus roundtrip SNR too low: %.2f dB corr=%.4f lag=%d", snr, corr, lag)
	}
}

func syntheticSpeechLikePCM24(n int) []int16 {
	out := make([]int16, n)
	phase1, phase2 := 0.0, 0.0
	for i := range out {
		t := float64(i) / 24000
		// Slowly varying voiced-ish source with an amplitude envelope and silence gaps.
		f1 := 120.0 + 35.0*math.Sin(2*math.Pi*1.7*t)
		f2 := 700.0 + 180.0*math.Sin(2*math.Pi*0.9*t)
		phase1 += 2 * math.Pi * f1 / 24000
		phase2 += 2 * math.Pi * f2 / 24000
		env := 0.35 + 0.65*math.Pow(math.Sin(math.Pi*3*t), 2)
		if int(t*4)%7 == 6 {
			env *= 0.08
		}
		x := env * (0.65*math.Sin(phase1) + 0.25*math.Sin(2*phase1) + 0.18*math.Sin(phase2))
		out[i] = int16(math.MaxInt16 * 0.45 * x)
	}
	return out
}

func bestCorrelation(a, b []int16, maxLag int) (best float64, bestLag int) {
	for lag := -maxLag; lag <= maxLag; lag++ {
		c := correlationAtLag(a, b, lag)
		if c > best {
			best = c
			bestLag = lag
		}
	}
	return best, bestLag
}

func correlationAtLag(a, b []int16, lag int) float64 {
	startA, startB := 0, 0
	if lag > 0 {
		startB = lag
	} else if lag < 0 {
		startA = -lag
	}
	n := min(len(a)-startA, len(b)-startB)
	if n <= 0 {
		return 0
	}
	var dot, aa, bb float64
	for i := 0; i < n; i++ {
		x := float64(a[startA+i])
		y := float64(b[startB+i])
		dot += x * y
		aa += x * x
		bb += y * y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / math.Sqrt(aa*bb)
}

func alignedSNRDB(a, b []int16, lag int) float64 {
	startA, startB := 0, 0
	if lag > 0 {
		startB = lag
	} else if lag < 0 {
		startA = -lag
	}
	n := min(len(a)-startA, len(b)-startB)
	if n <= 0 {
		return math.Inf(-1)
	}
	var sig, noise float64
	for i := 0; i < n; i++ {
		x := float64(a[startA+i])
		y := float64(b[startB+i])
		sig += x * x
		d := x - y
		noise += d * d
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}
