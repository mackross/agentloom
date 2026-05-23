package smartturn

import (
	"math"

	"gonum.org/v1/gonum/dsp/fourier"
)

const (
	SampleRate     = 16000
	ChunkSeconds   = 8
	NSamples       = SampleRate * ChunkSeconds
	NFFT           = 400
	HopLength      = 160
	FeatureSize    = 80
	NumFrames      = NSamples / HopLength
	NumFreqBins    = 1 + NFFT/2
	whisperEpsilon = 1e-7
)

type FeatureExtractor struct {
	window     []float64
	melFilters [][]float64 // [freq][mel]
	fft        *fourier.FFT
}

func NewFeatureExtractor() *FeatureExtractor {
	return &FeatureExtractor{
		window:     hannWindow(NFFT),
		melFilters: melFilterBank(NumFreqBins, FeatureSize, 0, 8000, SampleRate),
		fft:        fourier.NewFFT(NFFT),
	}
}

// Extract converts mono 16kHz float PCM into Whisper-compatible log-mel features
// for Smart Turn: flattened row-major float32[1, 80, 800]. Audio is truncated to
// the last 8s or left-padded with silence, then zero-mean/unit-variance normalized
// like WhisperFeatureExtractor(..., chunk_length=8, do_normalize=true).
func (e *FeatureExtractor) Extract(audio []float32) []float32 {
	wave := padOrTrimLast(audio, NSamples)
	normalizeInPlace(wave)
	padded := reflectPad(wave, NFFT/2)

	features := make([]float32, FeatureSize*NumFrames)
	frame := make([]float64, NFFT)
	coeff := make([]complex128, NFFT/2+1)
	power := make([]float64, NumFreqBins)
	mel := make([]float64, FeatureSize*NumFrames)

	maxLog := math.Inf(-1)
	for t := 0; t < NumFrames+1; t++ { // torch.stft yields 801; Whisper drops last.
		start := t * HopLength
		for i := 0; i < NFFT; i++ {
			frame[i] = float64(padded[start+i]) * e.window[i]
		}
		coeff = e.fft.Coefficients(coeff, frame)
		for i, c := range coeff {
			power[i] = real(c)*real(c) + imag(c)*imag(c)
		}
		if t == NumFrames {
			break
		}
		for m := 0; m < FeatureSize; m++ {
			v := 0.0
			for f := 0; f < NumFreqBins; f++ {
				v += power[f] * e.melFilters[f][m]
			}
			if v < 1e-10 {
				v = 1e-10
			}
			lv := math.Log10(v)
			mel[m*NumFrames+t] = lv
			if lv > maxLog {
				maxLog = lv
			}
		}
	}

	floor := maxLog - 8.0
	for i, v := range mel {
		if v < floor {
			v = floor
		}
		features[i] = float32((v + 4.0) / 4.0)
	}
	return features
}

func padOrTrimLast(in []float32, n int) []float32 {
	out := make([]float32, n)
	if len(in) >= n {
		copy(out, in[len(in)-n:])
	} else {
		copy(out[n-len(in):], in)
	}
	return out
}

func normalizeInPlace(x []float32) {
	mean := 0.0
	for _, v := range x {
		mean += float64(v)
	}
	mean /= float64(len(x))
	variance := 0.0
	for _, v := range x {
		d := float64(v) - mean
		variance += d * d
	}
	variance /= float64(len(x))
	scale := 1.0 / math.Sqrt(variance+whisperEpsilon)
	for i, v := range x {
		x[i] = float32((float64(v) - mean) * scale)
	}
}

func reflectPad(x []float32, pad int) []float32 {
	out := make([]float32, len(x)+2*pad)
	for i := 0; i < pad; i++ {
		out[i] = x[pad-i]
		out[pad+len(x)+i] = x[len(x)-2-i]
	}
	copy(out[pad:], x)
	return out
}

func hannWindow(n int) []float64 {
	w := make([]float64, n)
	// numpy.hanning(n+1)[:-1] / torch.hann_window(periodic=true)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}

func melFilterBank(numFrequencyBins, numMelFilters int, minFrequency, maxFrequency float64, samplingRate int) [][]float64 {
	melMin := hertzToMelSlaney(minFrequency)
	melMax := hertzToMelSlaney(maxFrequency)
	filterFreqs := make([]float64, numMelFilters+2)
	for i := range filterFreqs {
		mel := melMin + (melMax-melMin)*float64(i)/float64(numMelFilters+1)
		filterFreqs[i] = melToHertzSlaney(mel)
	}

	fftFreqs := make([]float64, numFrequencyBins)
	for i := range fftFreqs {
		fftFreqs[i] = float64(samplingRate/2) * float64(i) / float64(numFrequencyBins-1)
	}

	filters := make([][]float64, numFrequencyBins)
	for f := range filters {
		filters[f] = make([]float64, numMelFilters)
		for m := 0; m < numMelFilters; m++ {
			down := -(filterFreqs[m] - fftFreqs[f]) / (filterFreqs[m+1] - filterFreqs[m])
			up := (filterFreqs[m+2] - fftFreqs[f]) / (filterFreqs[m+2] - filterFreqs[m+1])
			v := math.Min(down, up)
			if v < 0 {
				v = 0
			}
			// Slaney area normalization.
			v *= 2.0 / (filterFreqs[m+2] - filterFreqs[m])
			filters[f][m] = v
		}
	}
	return filters
}

func hertzToMelSlaney(freq float64) float64 {
	mels := 3.0 * freq / 200.0
	if freq >= 1000.0 {
		mels = 15.0 + math.Log(freq/1000.0)*(27.0/math.Log(6.4))
	}
	return mels
}

func melToHertzSlaney(mels float64) float64 {
	freq := 200.0 * mels / 3.0
	if mels >= 15.0 {
		freq = 1000.0 * math.Exp((math.Log(6.4)/27.0)*(mels-15.0))
	}
	return freq
}
