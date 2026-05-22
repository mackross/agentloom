const $ = (id) => document.getElementById(id);

const connectButton = $('connect');
const micButton = $('mic');
const commitButton = $('commit');
const interruptButton = $('interrupt');
const summaryButton = $('summary');
const sendTextButton = $('sendText');
const textInput = $('text');
const statusEl = $('status');
const logEl = $('log');
const summaryBox = $('summaryBox');
const metricEls = {
  rtt: $('metric-rtt'),
  audioChunks: $('metric-audio-chunks'),
  vadStart: $('metric-vad-start'),
  stopResponse: $('metric-stop-response'),
  responseAudio: $('metric-response-audio'),
  stopAudio: $('metric-stop-audio'),
  playbackQueue: $('metric-playback-queue'),
  eventAge: $('metric-event-age'),
};

let ws;
let audioContext;
let micStream;
let micSource;
let micNode;
let micOn = false;
let playbackTime = 0;
let playbackSources = new Set();
let assistantAudioItems = new Map();
let pingTimer = null;
let audioChunksSent = 0;
let lastAudioChunkSentAt = 0;
let lastSpeechStartedAt = 0;
let lastSpeechStoppedAt = 0;
let lastResponseCreatedAt = 0;
let awaitingFirstAudio = false;
let summaryResponseIDs = new Set();

connectButton.onclick = connect;
micButton.onclick = async () => {
  try {
    await toggleMic();
  } catch (err) {
    const message = err && err.message ? err.message : String(err);
    setStatus('mic error');
    log(`MIC ERROR: ${message}`);
    report(`mic error: ${message}`);
  }
};
commitButton.onclick = () => send({ type: 'commit' });
interruptButton.onclick = () => {
  const truncation = currentAssistantTruncation();
  clearAssistantAudio();
  send({ type: 'interrupt', ...(truncation || {}) });
};
summaryButton.onclick = () => {
  appendSummary('\n\n--- summary requested ---\n');
  send({ type: 'summary' });
};
sendTextButton.onclick = sendText;
textInput.addEventListener('keydown', (event) => {
  if (event.key === 'Enter') sendText();
});

function connect() {
  if (ws && ws.readyState === WebSocket.OPEN) return;

  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${scheme}://${location.host}/voice`);

  ws.onopen = () => {
    setStatus('connected; starting OpenAI session...');
    connectButton.disabled = true;
    micButton.disabled = false;
    commitButton.disabled = false;
    interruptButton.disabled = false;
    summaryButton.disabled = false;
    sendTextButton.disabled = false;
    send({ type: 'start' });
    startPings();
  };

  ws.onmessage = (event) => {
    let msg;
    try {
      msg = JSON.parse(event.data);
    } catch (err) {
      log(`bad json from server: ${err}`);
      return;
    }
    handleServerEvent(msg);
  };

  ws.onclose = () => {
    setStatus('disconnected');
    connectButton.disabled = false;
    micButton.disabled = true;
    commitButton.disabled = true;
    interruptButton.disabled = true;
    summaryButton.disabled = true;
    sendTextButton.disabled = true;
    stopPings();
    stopMic();
  };

  ws.onerror = () => log('websocket error');
}

async function toggleMic() {
  if (micOn) {
    stopMic();
    return;
  }
  await startMic();
}

async function startMic() {
  report(`startMic clicked; secureContext=${window.isSecureContext}; mediaDevices=${!!navigator.mediaDevices}; getUserMedia=${!!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia)}`);
  if (!window.isSecureContext) {
    throw new Error('microphone capture requires a secure context. On iPhone, use HTTPS; http://192.168.x.x is not enough.');
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    throw new Error('navigator.mediaDevices.getUserMedia is unavailable in this browser/context.');
  }
  if (!ws || ws.readyState !== WebSocket.OPEN) {
    log('connect first');
    return;
  }
  if (!audioContext) {
    audioContext = new AudioContext();
  }
  if (!audioContext.audioWorklet) {
    throw new Error('AudioWorklet is unavailable in this browser/context.');
  }
  await audioContext.resume();
  report(`audio context ready; sampleRate=${audioContext.sampleRate}; state=${audioContext.state}`);
  await audioContext.audioWorklet.addModule('/static/mic-worklet.js');
  report('mic worklet loaded');

  micStream = await navigator.mediaDevices.getUserMedia({
    audio: {
      channelCount: 1,
      echoCancellation: true,
      noiseSuppression: true,
      autoGainControl: true,
    },
  });
  report('getUserMedia granted');
  micSource = audioContext.createMediaStreamSource(micStream);
  micNode = new AudioWorkletNode(audioContext, 'mic-processor', {
    processorOptions: { targetRate: 24000, chunkSize: 2400 },
  });
  micNode.port.onmessage = (event) => {
    if (!micOn || !event.data || event.data.type !== 'audio') return;
    const pcm = new Int16Array(event.data.buffer);
    audioChunksSent++;
    lastAudioChunkSentAt = performance.now();
    metricEls.audioChunks.textContent = String(audioChunksSent);
    send({ type: 'audio', data: int16ToBase64(pcm) });
  };

  // Some browsers do not process a graph unless it connects to the destination.
  // Keep output silent with a zero-gain node.
  const zeroGain = audioContext.createGain();
  zeroGain.gain.value = 0;
  micSource.connect(micNode);
  micNode.connect(zeroGain);
  zeroGain.connect(audioContext.destination);

  micOn = true;
  micButton.textContent = 'Pause Mic';
  setStatus('mic streaming');
  report('mic streaming started');
}

function stopMic() {
  micOn = false;
  if (micNode) {
    micNode.port.postMessage({ type: 'set-enabled', enabled: false });
    micNode.disconnect();
    micNode = null;
  }
  if (micSource) {
    micSource.disconnect();
    micSource = null;
  }
  if (micStream) {
    for (const track of micStream.getTracks()) track.stop();
    micStream = null;
  }
  micButton.textContent = 'Start Mic';
  if (ws && ws.readyState === WebSocket.OPEN) setStatus('mic paused');
}

function sendText() {
  const text = textInput.value.trim();
  if (!text) return;
  send({ type: 'text', text });
  textInput.value = '';
  log(`you(text)> ${text}`);
}

function handleServerEvent(msg) {
  updateEventAge(msg);
  switch (msg.type) {
    case 'pong':
      metricEls.rtt.textContent = `${Math.round(performance.now() - msg.client_time_ms)} ms`;
      break;
    case 'session.started':
      setStatus(micOn ? 'mic streaming' : 'session ready');
      log('session started');
      break;
    case 'user.audio.committed':
      break;
    case 'response.created':
      if (summaryResponseIDs.has(msg.response_id)) break;
      lastResponseCreatedAt = performance.now();
      awaitingFirstAudio = true;
      if (lastSpeechStoppedAt) {
        metricEls.stopResponse.textContent = formatMS(lastResponseCreatedAt - lastSpeechStoppedAt);
      }
      break;
    case 'summary.started':
      if (msg.response_id) summaryResponseIDs.add(msg.response_id);
      appendSummary('[summary started]\n');
      break;
    case 'summary.text.delta':
      appendSummary(msg.text || '');
      break;
    case 'summary.done':
      appendSummary('\n[summary done]\n');
      break;
    case 'user.transcript':
      log(`you> ${msg.text || ''}`);
      break;
    case 'assistant.text.delta':
      appendInline(msg.text || '');
      break;
    case 'assistant.audio.delta':
      if (awaitingFirstAudio) {
        const now = performance.now();
        metricEls.responseAudio.textContent = formatMS(now - lastResponseCreatedAt);
        if (lastSpeechStoppedAt) metricEls.stopAudio.textContent = formatMS(now - lastSpeechStoppedAt);
        awaitingFirstAudio = false;
      }
      if (msg.data) playPCM16Base64(msg.data, msg.item_id || '', msg.content_index ?? 0);
      break;
    case 'user.speech.started':
      lastSpeechStartedAt = performance.now();
      if (lastAudioChunkSentAt) metricEls.vadStart.textContent = formatMS(lastSpeechStartedAt - lastAudioChunkSentAt);
      {
        const truncation = currentAssistantTruncation();
        if (truncation) send({ type: 'truncate', ...truncation });
      }
      clearAssistantAudio();
      log('\n[barge-in detected: cleared assistant audio]');
      break;
    case 'user.speech.stopped':
      lastSpeechStoppedAt = performance.now();
      break;
    case 'assistant.cancelled':
      clearAssistantAudio();
      log('\n[assistant response cancelled]');
      break;
    case 'tool.call':
      log(`\ntool call ${msg.name} ${msg.arguments || ''}`);
      break;
    case 'tool.result':
      log(`tool result ${msg.name}: ${msg.output || ''}`);
      break;
    case 'error':
      log(`\nERROR: ${msg.message || 'unknown error'}`);
      break;
    case 'debug':
      if (msg.message) log(`debug: ${msg.message}`);
      break;
    default:
      if (msg.type) log(`event: ${msg.type}`);
      break;
  }
}

function send(obj) {
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  ws.send(JSON.stringify(obj));
}

function startPings() {
  stopPings();
  sendPing();
  pingTimer = setInterval(sendPing, 2000);
}

function stopPings() {
  if (pingTimer) clearInterval(pingTimer);
  pingTimer = null;
}

function sendPing() {
  send({ type: 'ping', client_time_ms: Math.round(performance.now()) });
}

function report(text) {
  log(`debug: ${text}`);
  send({ type: 'browser.log', text });
}

function setStatus(text) {
  statusEl.textContent = text;
}

function log(line) {
  logEl.textContent += `${line}\n`;
  logEl.scrollTop = logEl.scrollHeight;
}

function appendInline(text) {
  logEl.textContent += text;
  logEl.scrollTop = logEl.scrollHeight;
}

function appendSummary(text) {
  summaryBox.textContent += text;
  summaryBox.scrollTop = summaryBox.scrollHeight;
}

function int16ToBase64(samples) {
  const bytes = new Uint8Array(samples.buffer, samples.byteOffset, samples.byteLength);
  let binary = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(binary);
}

function base64ToInt16(base64) {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new Int16Array(bytes.buffer);
}

async function playPCM16Base64(base64, itemID, contentIndex) {
  if (!audioContext) audioContext = new AudioContext();
  await audioContext.resume();

  const pcm = base64ToInt16(base64);
  const audioBuffer = audioContext.createBuffer(1, pcm.length, 24000);
  const channel = audioBuffer.getChannelData(0);
  for (let i = 0; i < pcm.length; i++) {
    channel[i] = Math.max(-1, Math.min(1, pcm[i] / 32768));
  }

  const source = audioContext.createBufferSource();
  source.buffer = audioBuffer;
  source.connect(audioContext.destination);

  const now = audioContext.currentTime;
  if (playbackTime < now) playbackTime = now + 0.02;
  const queueDelay = Math.max(0, playbackTime - now);
  metricEls.playbackQueue.textContent = formatMS(queueDelay * 1000);
  const startTime = playbackTime;
  const endTime = startTime + audioBuffer.duration;
  source.start(startTime);
  playbackTime = endTime;

  let itemState = null;
  if (itemID) {
    const key = audioItemKey(itemID, contentIndex);
    itemState = assistantAudioItems.get(key);
    if (!itemState) {
      itemState = {
        item_id: itemID,
        content_index: contentIndex,
        first_start_time: startTime,
        last_end_time: endTime,
        total_duration: 0,
      };
      assistantAudioItems.set(key, itemState);
    }
    if (startTime < itemState.first_start_time) itemState.first_start_time = startTime;
    if (endTime > itemState.last_end_time) itemState.last_end_time = endTime;
    itemState.total_duration += audioBuffer.duration;
  }

  playbackSources.add(source);
  source.onended = () => playbackSources.delete(source);
}

function clearAssistantAudio() {
  for (const source of playbackSources) {
    try {
      source.stop();
    } catch (_) {
      // Already stopped.
    }
  }
  playbackSources.clear();
  assistantAudioItems.clear();
  playbackTime = audioContext ? audioContext.currentTime : 0;
}

function currentAssistantTruncation() {
  if (!audioContext || assistantAudioItems.size === 0) return null;
  const now = audioContext.currentTime;
  let candidate = null;

  for (const item of assistantAudioItems.values()) {
    if (now >= item.first_start_time && now <= item.last_end_time) {
      if (!candidate || item.first_start_time > candidate.first_start_time) candidate = item;
    }
  }
  if (!candidate) {
    for (const item of assistantAudioItems.values()) {
      if (now >= item.first_start_time) {
        if (!candidate || item.first_start_time > candidate.first_start_time) candidate = item;
      }
    }
  }
  if (!candidate) return null;

  const heardSeconds = Math.max(0, Math.min(candidate.total_duration, now - candidate.first_start_time));
  return {
    item_id: candidate.item_id,
    content_index: candidate.content_index,
    audio_end_ms: Math.round(heardSeconds * 1000),
  };
}

function audioItemKey(itemID, contentIndex) {
  return `${itemID}:${contentIndex}`;
}

function updateEventAge(msg) {
  if (!msg.server_time_ms) return;
  // This includes browser/server clock skew, so it is only a rough freshness
  // signal. Use the explicit ping RTT for actual network RTT.
  const age = Date.now() - msg.server_time_ms;
  metricEls.eventAge.textContent = `${Math.round(age)} ms`;
}

function formatMS(ms) {
  if (!Number.isFinite(ms)) return '—';
  return `${Math.max(0, Math.round(ms))} ms`;
}
