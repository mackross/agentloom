class MicProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const opts = options.processorOptions || {};
    this.targetRate = opts.targetRate || 24000;
    this.sourceRate = sampleRate;
    this.ratio = this.sourceRate / this.targetRate;
    this.pending = [];
    this.pendingFrames = 0;
    this.phase = 0;
    this.chunkSize = opts.chunkSize || 2400; // 100ms at 24kHz.
    this.out = [];
    this.enabled = true;

    this.port.onmessage = (event) => {
      if (event.data && event.data.type === 'set-enabled') {
        this.enabled = !!event.data.enabled;
      }
    };
  }

  process(inputs) {
    const input = inputs[0];
    if (!this.enabled || !input || !input[0] || input[0].length === 0) {
      return true;
    }

    const channel = input[0];
    // Linear-ish downsampling by source index. Good enough for the spike; a
    // production path should use a better low-pass/downsampler.
    while (this.phase < channel.length) {
      const sample = channel[Math.floor(this.phase)] || 0;
      const clipped = Math.max(-1, Math.min(1, sample));
      const pcm = clipped < 0 ? clipped * 0x8000 : clipped * 0x7fff;
      this.out.push(pcm | 0);
      this.phase += this.ratio;

      if (this.out.length >= this.chunkSize) {
        this.flush();
      }
    }
    this.phase -= channel.length;
    return true;
  }

  flush() {
    if (this.out.length === 0) return;
    const chunk = new Int16Array(this.out);
    this.out = [];
    this.port.postMessage({ type: 'audio', buffer: chunk.buffer }, [chunk.buffer]);
  }
}

registerProcessor('mic-processor', MicProcessor);
