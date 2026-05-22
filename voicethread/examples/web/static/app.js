const $ = (id) => document.getElementById(id);

const connectButton = $('connect');
const disconnectButton = $('disconnect');
const cancelButton = $('cancel');
const sendTextButton = $('sendText');
const textInput = $('text');
const statusEl = $('status');
const logEl = $('log');
const remoteAudio = $('remoteAudio');

let pc;
let dc;
let micStream;
let callId = '';

connectButton.onclick = connect;
disconnectButton.onclick = disconnect;
cancelButton.onclick = serverCancel;
sendTextButton.onclick = sendText;
textInput.addEventListener('keydown', (event) => {
  if (event.key === 'Enter') sendText();
});

async function connect() {
  try {
    setStatus('requesting ephemeral session...');
    const tokenResp = await fetch('/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    if (!tokenResp.ok) throw new Error(await tokenResp.text());
    const tokenData = await tokenResp.json();
    const ephemeralKey = tokenData.value || (tokenData.client_secret && tokenData.client_secret.value);
    if (!ephemeralKey) throw new Error('OpenAI client_secret response did not include value');

    setStatus('opening microphone...');
    micStream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });

    pc = new RTCPeerConnection();
    pc.ontrack = (event) => {
      remoteAudio.srcObject = event.streams[0];
    };
    pc.onconnectionstatechange = () => {
      log(`pc.connectionState=${pc.connectionState}`);
      if (['closed', 'failed', 'disconnected'].includes(pc.connectionState)) setDisconnectedUI();
    };

    for (const track of micStream.getTracks()) pc.addTrack(track, micStream);

    dc = pc.createDataChannel('oai-events');
    dc.onopen = () => {
      log('data channel open');
      sendTextButton.disabled = false;
    };
    dc.onmessage = (event) => handleRealtimeEvent(event.data);
    dc.onerror = () => log('data channel error');

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);

    setStatus('posting SDP offer to OpenAI...');
    const sdpResp = await fetch('https://api.openai.com/v1/realtime/calls', {
      method: 'POST',
      body: offer.sdp,
      headers: {
        Authorization: `Bearer ${ephemeralKey}`,
        'Content-Type': 'application/sdp',
      },
    });
    if (!sdpResp.ok) throw new Error(await sdpResp.text());
    const location = sdpResp.headers.get('Location') || '';
    callId = location.split('/').pop() || '';
    log(`OpenAI call_id=${callId || '(missing Location header)'}`);

    const answer = { type: 'answer', sdp: await sdpResp.text() };
    await pc.setRemoteDescription(answer);

    if (callId) {
      setStatus('joining server sideband...');
      const sidebandResp = await fetch('/sideband', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ call_id: callId }),
      });
      if (!sidebandResp.ok) throw new Error(await sidebandResp.text());
      log('server sideband connected');
    } else {
      log('WARNING: no call_id; server sideband could not join');
    }

    connectButton.disabled = true;
    disconnectButton.disabled = false;
    cancelButton.disabled = !callId;
    setStatus('connected; speak naturally');
  } catch (err) {
    const message = err && err.message ? err.message : String(err);
    setStatus('connect error');
    log(`CONNECT ERROR: ${message}`);
    report(`connect error: ${message}`);
    disconnect();
  }
}

function disconnect() {
  if (dc) {
    dc.close();
    dc = null;
  }
  if (pc) {
    pc.close();
    pc = null;
  }
  if (micStream) {
    for (const track of micStream.getTracks()) track.stop();
    micStream = null;
  }
  remoteAudio.srcObject = null;
  callId = '';
  setDisconnectedUI();
}

async function serverCancel() {
  if (!callId) return;
  try {
    const resp = await fetch('/cancel', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ call_id: callId }),
    });
    if (!resp.ok) throw new Error(await resp.text());
    log('server sent response.cancel');
  } catch (err) {
    log(`cancel error: ${err && err.message ? err.message : String(err)}`);
  }
}

function sendText() {
  const text = textInput.value.trim();
  if (!text || !dc || dc.readyState !== 'open') return;
  sendRealtime({
    type: 'conversation.item.create',
    item: { type: 'message', role: 'user', content: [{ type: 'input_text', text }] },
  });
  sendRealtime({ type: 'response.create' });
  log(`you(text)> ${text}`);
  textInput.value = '';
}

function sendRealtime(event) {
  dc.send(JSON.stringify(event));
}

function handleRealtimeEvent(raw) {
  let msg;
  try {
    msg = JSON.parse(raw);
  } catch {
    log(`raw> ${truncate(raw, 500)}`);
    return;
  }
  switch (msg.type) {
    case 'session.created':
    case 'session.updated':
    case 'input_audio_buffer.speech_started':
    case 'input_audio_buffer.speech_stopped':
    case 'response.created':
    case 'response.done':
      log(`${msg.type}${msg.response ? ` status=${msg.response.status || ''}` : ''}`);
      break;
    case 'response.audio_transcript.delta':
    case 'response.output_audio_transcript.delta':
    case 'response.text.delta':
    case 'response.output_text.delta':
      log(`assistant> ${msg.delta || ''}`);
      break;
    case 'error':
      log(`ERROR ${msg.error && msg.error.code ? msg.error.code : ''}: ${msg.error && msg.error.message ? msg.error.message : JSON.stringify(msg)}`);
      break;
    default:
      if (!String(msg.type || '').includes('audio.delta')) log(msg.type || truncate(raw, 300));
  }
}

function setDisconnectedUI() {
  connectButton.disabled = false;
  disconnectButton.disabled = true;
  cancelButton.disabled = true;
  sendTextButton.disabled = true;
  setStatus('disconnected');
}

function setStatus(text) {
  statusEl.textContent = text;
}

function log(text) {
  const line = `[${new Date().toLocaleTimeString()}] ${text}`;
  logEl.textContent += `${line}\n`;
  logEl.scrollTop = logEl.scrollHeight;
  report(text);
}

function report(text) {
  fetch('/log', { method: 'POST', body: text }).catch(() => {});
}

function truncate(s, n) {
  s = String(s || '');
  return s.length <= n ? s : s.slice(0, n - 1) + '…';
}
