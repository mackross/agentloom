package main

import (
	"encoding/binary"
	"flag"
	"log"
	"math"
	"os"

	"github.com/mackross/agentloom/voicethread/smartturn"
)

func main() {
	out := flag.String("out", "/tmp/smartturn-go-features.bin", "output raw float32 file")
	flag.Parse()

	audio := make([]float32, smartturn.SampleRate)
	for i := range audio {
		audio[i] = float32(0.1 * math.Sin(2*math.Pi*440*float64(i)/smartturn.SampleRate))
	}
	features := smartturn.NewFeatureExtractor().Extract(audio)

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := binary.Write(f, binary.LittleEndian, features); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d features", len(features))
}
