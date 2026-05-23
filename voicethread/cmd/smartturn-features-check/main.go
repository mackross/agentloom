package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"sort"

	"github.com/mackross/agentloom/voicethread/smartturn"
)

func main() {
	audioPath := flag.String("audio", "/tmp/smartturn-audio.bin", "raw float32 mono 16kHz audio from scripts/smartturn_golden.py")
	goldenPath := flag.String("golden", "/tmp/smartturn-py-features.bin", "raw float32[80,800] Python features")
	flag.Parse()

	audio := readFloat32s(*audioPath)
	golden := readFloat32s(*goldenPath)
	got := smartturn.NewFeatureExtractor().Extract(audio)
	if len(golden) != len(got) {
		log.Fatalf("length mismatch golden=%d got=%d", len(golden), len(got))
	}

	abs := make([]float64, len(got))
	var max, sum float64
	var maxI int
	for i := range got {
		d := math.Abs(float64(got[i] - golden[i]))
		abs[i] = d
		sum += d
		if d > max {
			max = d
			maxI = i
		}
	}
	sort.Float64s(abs)
	q := func(p float64) float64 { return abs[int(p*float64(len(abs)-1))] }
	fmt.Printf("count=%d\n", len(got))
	fmt.Printf("max_abs=%g at [%d,%d] go=%g py=%g\n", max, maxI/smartturn.NumFrames, maxI%smartturn.NumFrames, got[maxI], golden[maxI])
	fmt.Printf("mean_abs=%g\n", sum/float64(len(abs)))
	fmt.Printf("p50_abs=%g p95_abs=%g p99_abs=%g p999_abs=%g\n", q(.50), q(.95), q(.99), q(.999))
	for _, idx := range []int{0, 1, 2, 799, 800, 801, 12345, len(got) - 1} {
		fmt.Printf("sample[%d]=go:% .8f py:% .8f diff:%g\n", idx, got[idx], golden[idx], got[idx]-golden[idx])
	}
}

func readFloat32s(path string) []float32 {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		log.Fatal(err)
	}
	if len(b)%4 != 0 {
		log.Fatalf("%s: byte length %d is not multiple of 4", path, len(b))
	}
	out := make([]float32, len(b)/4)
	r := bytesReader(b)
	if err := binary.Read(&r, binary.LittleEndian, out); err != nil {
		log.Fatal(err)
	}
	return out
}

type bytesReader []byte

func (r *bytesReader) Read(p []byte) (int, error) {
	if len(*r) == 0 {
		return 0, io.EOF
	}
	n := copy(p, *r)
	*r = (*r)[n:]
	return n, nil
}
