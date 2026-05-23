const $ = (id) => document.getElementById(id);

const connectButton = $('connect');
const micButton = $('mic');
const commitButton = $('commit');
const interruptButton = $('interrupt');
const continueButton = $('continue');
const summaryButton = $('summary');
const sendTextButton = $('sendText');
const textInput = $('text');
const statusEl = $('status');
const logEl = $('log');
const summaryBox = $('summaryBox');
const remoteAudio = $('remoteAudio');
const metricEls = {
  rtt: $('metric-rtt'),
  vadStart: $('metric-vad-start'),
  stopResponse: $('metric-stop-response'),
  responseAudio: $('metric-response-audio'),
  eventAge: $('metric-event-age'),
};

let pc;
let dc;
let controlDc;
let micStream;
let micPaused = false;
let pingTimer = null;
let lastSpeechStartedAt = 0;
let lastSpeechStoppedAt = 0;
let lastResponseCreatedAt = 0;
let awaitingFirstAudio = false;
let summaryResponseIDs = new Set();

connectButton.onclick = connect;
micButton.onclick = toggleMic;
commitButton.onclick = () => send({ type: 'commit' });
interruptButton.onclick = () => send({ type: 'interrupt' });
continueButton.onclick = () => {
  send({ type: 'continue' });
  log('continue requested');
};
summaryButton.onclick = () => {
  appendSummary('\n\n--- summary requested ---\n');
  send({ type: 'summary' });
};
sendTextButton.onclick = sendText;
textInput.addEventListener('keydown', (event) => {
  if (event.key === 'Enter') sendText();
});

async function connect() {
  try {
    if (pc) return;
    setStatus('opening microphone...');
    if (!window.isSecureContext) throw new Error('microphone capture requires HTTPS or localhost');
    micStream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });

    pc = new RTCPeerConnection();
    pc.ontrack = (event) => {
      log(`remote track ${event.track.kind} ${event.track.id}`);
      remoteAudio.srcObject = event.streams[0];
    };
    pc.onconnectionstatechange = () => {
      log(`pc.connectionState=${pc.connectionState}`);
      if (['failed', 'closed', 'disconnected'].includes(pc.connectionState)) disconnect();
    };
    pc.oniceconnectionstatechange = () => log(`pc.iceConnectionState=${pc.iceConnectionState}`);

    for (const track of micStream.getTracks()) pc.addTrack(track, micStream);
    dc = pc.createDataChannel('oai-events');
    controlDc = pc.createDataChannel('control', { ordered: false, maxRetransmits: 0 });
    controlDc.onopen = () => log('control data channel open');
    controlDc.onclose = () => log('control data channel close');
    controlDc.onerror = () => log('control data channel error');
    dc.onopen = () => {
      log('data channel open');
      setConnectedUI();
      startPings();
    };
    dc.onmessage = (event) => handleServerEvent(JSON.parse(event.data));
    dc.onclose = () => {
      log('data channel close');
      disconnect();
    };
    dc.onerror = () => log('data channel error');

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    setStatus('connecting WebRTC to Go server...');
    const resp = await fetch('/rtc', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: offer.type, sdp: offer.sdp }),
    });
    if (!resp.ok) throw new Error(await resp.text());
    const answer = await resp.json();
    await pc.setRemoteDescription(answer);
  } catch (err) {
    const message = err && err.message ? err.message : String(err);
    setStatus('connect error');
    log(`CONNECT ERROR: ${message}`);
    disconnect();
  }
}

function disconnect() {
  stopPings();
  if (dc) {
    try { dc.close(); } catch {}
    dc = null;
  }
  if (controlDc) {
    try { controlDc.close(); } catch {}
    controlDc = null;
  }
  if (pc) {
    try { pc.close(); } catch {}
    pc = null;
  }
  if (micStream) {
    for (const track of micStream.getTracks()) track.stop();
    micStream = null;
  }
  remoteAudio.srcObject = null;
  micPaused = false;
  setDisconnectedUI();
}

function setConnectedUI() {
  connectButton.disabled = true;
  micButton.disabled = false;
  commitButton.disabled = false;
  interruptButton.disabled = false;
  continueButton.disabled = false;
  summaryButton.disabled = false;
  sendTextButton.disabled = false;
  setStatus('connected; speak naturally');
}

function setDisconnectedUI() {
  connectButton.disabled = false;
  micButton.disabled = true;
  commitButton.disabled = true;
  interruptButton.disabled = true;
  continueButton.disabled = true;
  summaryButton.disabled = true;
  sendTextButton.disabled = true;
  micButton.textContent = 'Pause Mic';
  setStatus('disconnected');
}

function toggleMic() {
  if (!micStream) return;
  micPaused = !micPaused;
  for (const track of micStream.getAudioTracks()) track.enabled = !micPaused;
  micButton.textContent = micPaused ? 'Resume Mic' : 'Pause Mic';
  setStatus(micPaused ? 'connected; mic paused' : 'connected; speak naturally');
  log(micPaused ? 'mic paused' : 'mic resumed');
}

function sendText() {
  const text = textInput.value.trim();
  if (!text) return;
  send({ type: 'text', text });
  textInput.value = '';
  log(`you(text)> ${text}`);
}

function send(msg) {
  const channel = controlDc && controlDc.readyState === 'open' ? controlDc : dc;
  if (!channel || channel.readyState !== 'open') return;
  channel.send(JSON.stringify(msg));
}

function handleServerEvent(msg) {
  updateEventAge(msg);
  switch (msg.type) {
    case 'pong':
      metricEls.rtt.textContent = `${Math.round(performance.now() - msg.client_time_ms)} ms`;
      break;
    case 'session.started':
      log('session started');
      break;
    case 'user.speech.started':
      lastSpeechStartedAt = performance.now();
      metricEls.vadStart.textContent = new Date().toLocaleTimeString();
      log('user speech started');
      break;
    case 'user.speech.stopped':
      lastSpeechStoppedAt = performance.now();
      log('user speech stopped');
      break;
    case 'response.created':
      if (summaryResponseIDs.has(msg.response_id)) break;
      lastResponseCreatedAt = performance.now();
      awaitingFirstAudio = true;
      if (lastSpeechStoppedAt) metricEls.stopResponse.textContent = `${Math.round(lastResponseCreatedAt - lastSpeechStoppedAt)} ms`;
      log(`response created ${msg.response_id || ''}`);
      break;
    case 'assistant.text.delta':
      if (summaryResponseIDs.has(msg.response_id)) break;
      if (awaitingFirstAudio) {
        awaitingFirstAudio = false;
        const now = performance.now();
        if (lastResponseCreatedAt) metricEls.responseAudio.textContent = `${Math.round(now - lastResponseCreatedAt)} ms`;
      }
      log(`assistant(audio transcript)> ${msg.text || ''}`);
      break;
    case 'assistant.cancelled':
      log('assistant cancelled');
      break;
    case 'truncate.sent':
      log(`truncate sent item=${msg.item_id || ''} content=${msg.content_index ?? ''} audio_end_ms=${msg.audio_end_ms ?? ''}`);
      break;
    case 'assistant.audio.truncated':
      log(`truncate ack item=${msg.item_id || ''} content=${msg.content_index ?? ''} audio_end_ms=${msg.audio_end_ms ?? ''}`);
      break;
    case 'response.done':
      log(`response done ${msg.response_id || ''}`);
      break;
    case 'user.transcript':
      log(`you(audio)> ${msg.text || ''}`);
      break;
    case 'summary.started':
      if (msg.response_id) summaryResponseIDs.add(msg.response_id);
      appendSummary('\n');
      break;
    case 'summary.text.delta':
      appendSummary(msg.text || '');
      break;
    case 'summary.done':
      appendSummary('\n');
      break;
    case 'debug':
      if (msg.message && !String(msg.message).includes('audio.delta')) log(`debug: ${msg.message}`);
      break;
    case 'error':
      log(`ERROR: ${msg.message || JSON.stringify(msg)}`);
      break;
    default:
      log(`${msg.type}${msg.text ? `: ${msg.text}` : ''}`);
  }
}

function startPings() {
  stopPings();
  pingTimer = setInterval(() => send({ type: 'ping', client_time_ms: performance.now() }), 2000);
}

function stopPings() {
  if (pingTimer) clearInterval(pingTimer);
  pingTimer = null;
}

function updateEventAge(msg) {
  if (!msg.server_time_ms) return;
  metricEls.eventAge.textContent = `${Math.max(0, Date.now() - msg.server_time_ms)} ms`;
}

function appendSummary(text) {
  summaryBox.textContent += text;
}

function setStatus(text) {
  statusEl.textContent = text;
}

function log(text) {
  const line = `[${new Date().toLocaleTimeString()}] ${text}`;
  logEl.textContent += `${line}\n`;
  logEl.scrollTop = logEl.scrollHeight;
}
