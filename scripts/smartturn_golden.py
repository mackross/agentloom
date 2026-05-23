#!/usr/bin/env python3
import argparse
import numpy as np
from transformers import WhisperFeatureExtractor

p = argparse.ArgumentParser()
p.add_argument('--audio-out', default='/tmp/smartturn-audio.bin')
p.add_argument('--features-out', default='/tmp/smartturn-py-features.bin')
p.add_argument('--seed', type=int, default=1234)
p.add_argument('--seconds', type=float, default=3.25)
args = p.parse_args()

sr = 16000
n = int(sr * args.seconds)
t = np.arange(n, dtype=np.float32) / sr
rng = np.random.default_rng(args.seed)
audio = (
    0.08 * np.sin(2 * np.pi * 220 * t)
    + 0.04 * np.sin(2 * np.pi * 997 * t)
    + 0.005 * rng.standard_normal(n).astype(np.float32)
).astype(np.float32)
# Add a little envelope so it is not a perfectly stationary signal.
audio *= np.linspace(0.2, 1.0, n, dtype=np.float32)

model_audio = np.zeros(8 * sr, dtype=np.float32)
if len(audio) >= len(model_audio):
    model_audio[:] = audio[-len(model_audio):]
else:
    model_audio[-len(audio):] = audio

extractor = WhisperFeatureExtractor(chunk_length=8)
inputs = extractor(
    model_audio,
    sampling_rate=sr,
    return_tensors='np',
    padding='max_length',
    max_length=8 * sr,
    truncation=True,
    do_normalize=True,
)
features = inputs.input_features.squeeze(0).astype(np.float32)
assert features.shape == (80, 800), features.shape

audio.tofile(args.audio_out)
features.tofile(args.features_out)
print('audio', audio.shape, args.audio_out)
print('features', features.shape, args.features_out)
print('features min/max/mean', float(features.min()), float(features.max()), float(features.mean()))
