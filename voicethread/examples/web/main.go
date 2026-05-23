package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	kopus "github.com/kazzmir/opus-go/opus"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	"github.com/mackross/agentloom/voicethread/smartturn"
	"github.com/mackross/agentloom/voicethread"
	pionopus "github.com/pion/opus"
	resampling "github.com/tphakala/go-audio-resampler"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

//go:embed static/*
var staticFS embed.FS

type clientMessage struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	ItemID       string `json:"item_id,omitempty"`
	ContentIndex *int   `json:"content_index,omitempty"`
	AudioEndMS   int    `json:"audio_end_ms,omitempty"`
	ClientTimeMS int64  `json:"client_time_ms,omitempty"`
}

// HACK1: post-response.done truncate workaround. Undo: delete assistantItemState and all itemStates plumbing, then rely only on conversation.item.truncate.
type assistantItemState struct {
	previousItemID string
	status         string
	transcript     string
}

// HACK1: post-response.done truncate workaround. Undo: delete assistantAudioState and all assistantAudio plumbing once completed-item truncation reliably updates context.
type assistantAudioState struct {
	contentIndex int
	pcm24        []int16
}

type rtcOffer struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

type rtcAnswer struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.Handle("/static/", noStore(http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/rtc", serveRTC)

	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	log.Printf("voice WebRTC bridge example listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func serveRTC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var offer rtcOffer
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if offer.SDP == "" {
		http.Error(w, "missing sdp", http.StatusBadRequest)
		return
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		// WebRTC Opus is negotiated as opus/48000/2 in browsers, even when the
		// encoded audio is mono. Advertising opus/48000/1 makes Chrome reject the
		// answer with "codec is not supported by remote".
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	outputCodec := strings.ToLower(envDefault("VOICE_OUTPUT_CODEC", "opus"))
	if outputCodec != "opus" {
		outputCodec = "pcmu"
	}
	log.Printf("rtc output codec=%s", outputCodec)
	bridge := newAudioBridge(outputCodec)
	var session *voicethread.VoiceSession
	var dc *webrtc.DataChannel
	var dcMu sync.Mutex
	// HACK1: post-response.done truncate workaround. Undo: remove itemStates/itemStatesMu and rememberAssistantItemState calls.
	itemStates := map[string]assistantItemState{}
	var itemStatesMu sync.Mutex
	// HACK1: post-response.done truncate workaround. Undo: remove assistantAudio/assistantAudioMu and storeAssistantAudio calls.
	assistantAudio := map[string]assistantAudioState{}
	var assistantAudioMu sync.Mutex

	closeAll := func() {
		cancel()
		bridge.Close()
		if session != nil {
			_ = session.Close()
		}
		_ = pc.Close()
	}

	writeEvent := func(ev voicethread.Event) {
		if ev.ServerTimeMS == 0 {
			ev.ServerTimeMS = time.Now().UnixMilli()
		}
		logVoiceEvent(ev)
		// HACK1: post-response.done truncate workaround. Undo: remove this state tracker call.
		rememberAssistantItemState(itemStates, &itemStatesMu, ev)
		if ev.Type == "response.created" {
			bridge.ResumeOutput()
		}
		if ev.Type == "assistant.cancelled" {
			bridge.ClearOutput()
		}
		if ev.Type == "assistant.audio.delta" && ev.Data != "" {
			contentIndex := 0
			if ev.ContentIndex != nil {
				contentIndex = *ev.ContentIndex
			}
			// HACK1: post-response.done truncate workaround. Undo: remove assistant audio capture for transcription repair.
			storeAssistantAudio(assistantAudio, &assistantAudioMu, ev.ItemID, contentIndex, ev.Data)
			bridge.EnqueueOpenAIAudio(outputAudioChunk{
				data:         ev.Data,
				itemID:       ev.ItemID,
				contentIndex: contentIndex,
			})
			// Do not forward audio chunk events to the browser. Audio is sent over
			// the WebRTC media track, and forwarding every audio delta can congest
			// the SCTP association enough to delay browser->server interrupt
			// commands.
			return
		}
		b, err := json.Marshal(ev)
		if err != nil {
			log.Printf("marshal event: %v", err)
			return
		}
		dcMu.Lock()
		defer dcMu.Unlock()
		if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
			if err := dc.SendText(string(b)); err != nil {
				log.Printf("write browser data channel: %v", err)
			}
		}
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		http.Error(w, "set OPENAI_API_KEY before running the example", http.StatusInternalServerError)
		_ = pc.Close()
		return
	}
	session = voicethread.New(voicethread.Options{
		APIKey:                apiKey,
		Model:                 envDefault("OPENAI_REALTIME_MODEL", "gpt-realtime-2"),
		Voice:                 envDefault("OPENAI_REALTIME_VOICE", "marin"),
		TranscriptionLanguage: "en",
		Instructions:          "You are a concise voice assistant. You can use tools for time and echo. Speak naturally and briefly.",
		ToolRuntime:           voicethread.NewCatalogRuntime(exampleTools()),
		OnEvent:               writeEvent,
	})
	if err := session.Start(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		_ = pc.Close()
		return
	}
	bridge.Start(ctx, session, writeEvent, itemStates, &itemStatesMu, assistantAudio, &assistantAudioMu)

	outCap := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	if outputCodec == "opus" {
		outCap = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	}
	outTrack, err := webrtc.NewTrackLocalStaticSample(outCap, "assistant", "agentloom")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		closeAll()
		return
	}
	if _, err := pc.AddTrack(outTrack); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		closeAll()
		return
	}
	bridge.SetOutputTrack(outTrack)

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("rtc ice state: %s", state.String())
		writeEvent(voicethread.Event{Type: "debug", Message: "rtc ice state: " + state.String()})
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("rtc peer state: %s", state.String())
		writeEvent(voicethread.Event{Type: "debug", Message: "rtc peer state: " + state.String()})
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			go closeAll()
		}
	})
	pc.OnDataChannel(func(c *webrtc.DataChannel) {
		log.Printf("rtc data channel: %s", c.Label())
		if c.Label() == "oai-events" {
			dcMu.Lock()
			dc = c
			dcMu.Unlock()
			c.OnOpen(func() {
				writeEvent(voicethread.Event{Type: "session.started"})
			})
		}
		c.OnMessage(func(msg webrtc.DataChannelMessage) {
			// HACK1: post-response.done truncate workaround. Undo: call handleClientMessage without itemStates/itemStatesMu.
			handleClientMessage(ctx, session, bridge, writeEvent, itemStates, &itemStatesMu, assistantAudio, &assistantAudioMu, msg.Data)
		})
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		log.Printf("rtc audio track codec=%s", track.Codec().MimeType)
		go bridge.ReadBrowserAudio(ctx, track)
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		closeAll()
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		closeAll()
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		closeAll()
		return
	}
	<-gatherComplete

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rtcAnswer{Type: "answer", SDP: pc.LocalDescription().SDP})
}

// HACK1: post-response.done truncate workaround. Undo: remove itemStates/itemStatesMu parameters and maybeReplaceCompletedAssistantItem call.
func handleClientMessage(ctx context.Context, session *voicethread.VoiceSession, bridge *audioBridge, writeEvent func(voicethread.Event), itemStates map[string]assistantItemState, itemStatesMu *sync.Mutex, assistantAudio map[string]assistantAudioState, assistantAudioMu *sync.Mutex, b []byte) {
	var msg clientMessage
	if err := json.Unmarshal(b, &msg); err != nil {
		writeEvent(voicethread.Event{Type: "error", Message: "bad browser message: " + err.Error()})
		return
	}
	switch msg.Type {
	case "start":
		writeEvent(voicethread.Event{Type: "debug", Message: "session already started"})
	case "commit":
		if err := session.CommitAudio(ctx); err != nil {
			writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
		}
	case "text":
		if msg.Text != "" {
			if err := session.SendText(ctx, msg.Text); err != nil {
				writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
			}
		}
	case "summary":
		if err := session.SummarizeSinceLast(ctx); err != nil {
			writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
		}
	case "continue":
		if err := session.SendText(ctx, "Please continue from exactly where you were interrupted. Do not restart from the beginning; continue with the next words or next item after the last audible assistant text."); err != nil {
			writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
		}
	case "interrupt":
		interruptAssistant(ctx, session, bridge, writeEvent, itemStates, itemStatesMu, assistantAudio, assistantAudioMu)
		if msg.ItemID != "" && msg.ContentIndex != nil {
			log.Printf("browser interrupt truncate item_id=%q content_index=%d audio_end_ms=%d", msg.ItemID, *msg.ContentIndex, msg.AudioEndMS)
			if err := session.TruncateAssistantAudio(ctx, msg.ItemID, *msg.ContentIndex, msg.AudioEndMS); err != nil {
				writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
			}
		}
	case "truncate":
		if msg.ItemID != "" && msg.ContentIndex != nil {
			log.Printf("browser truncate item_id=%q content_index=%d audio_end_ms=%d", msg.ItemID, *msg.ContentIndex, msg.AudioEndMS)
			if err := session.TruncateAssistantAudio(ctx, msg.ItemID, *msg.ContentIndex, msg.AudioEndMS); err != nil {
				writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
			}
		}
	case "browser.log":
		log.Printf("browser log: %s", msg.Text)
	case "ping":
		writeEvent(voicethread.Event{Type: "pong", ClientTimeMS: msg.ClientTimeMS, ServerTimeMS: time.Now().UnixMilli()})
	default:
		writeEvent(voicethread.Event{Type: "error", Message: "unknown browser message type: " + msg.Type})
	}
}

// HACK1: post-response.done truncate workaround. Undo: remove itemStates/assistantAudio params and maybeReplaceCompletedAssistantItem call.
func interruptAssistant(ctx context.Context, session *voicethread.VoiceSession, bridge *audioBridge, writeEvent func(voicethread.Event), itemStates map[string]assistantItemState, itemStatesMu *sync.Mutex, assistantAudio map[string]assistantAudioState, assistantAudioMu *sync.Mutex) {
	stop := bridge.StopOutput(ctx)
	if stop.ok {
		log.Printf("stopped-output interrupt truncate item_id=%q content_index=%d audio_end_ms=%d", stop.itemID, stop.contentIndex, stop.audioEndMS)
		if err := session.TruncateAssistantAudio(ctx, stop.itemID, stop.contentIndex, stop.audioEndMS); err != nil {
			writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
		} else {
			writeEvent(voicethread.Event{
				Type:         "truncate.sent",
				ItemID:       stop.itemID,
				ContentIndex: &stop.contentIndex,
				AudioEndMS:   stop.audioEndMS,
			})
			maybeReplaceCompletedAssistantItem(ctx, session, writeEvent, itemStates, itemStatesMu, assistantAudio, assistantAudioMu, stop.itemID, stop.contentIndex, stop.audioEndMS)
		}
	}
	if err := session.Interrupt(ctx); err != nil {
		writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
	}
}

// HACK1: post-response.done truncate workaround. Undo: delete rememberAssistantItemState, maybeReplaceCompletedAssistantItem, and transcriptPrefixForMS.
func rememberAssistantItemState(states map[string]assistantItemState, mu *sync.Mutex, ev voicethread.Event) {
	if ev.Raw == nil || ev.Message != "conversation.item.done" && ev.Message != "conversation.item.added" {
		return
	}
	var raw struct {
		PreviousItemID string `json:"previous_item_id"`
		Item           struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Status  string `json:"status"`
			Content []struct {
				Type       string `json:"type"`
				Transcript string `json:"transcript"`
				Text       string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(ev.Raw, &raw); err != nil || raw.Item.ID == "" || raw.Item.Role != "assistant" {
		return
	}
	st := assistantItemState{previousItemID: raw.PreviousItemID, status: raw.Item.Status}
	for _, c := range raw.Item.Content {
		if c.Transcript != "" {
			st.transcript = c.Transcript
			break
		}
		if c.Text != "" {
			st.transcript = c.Text
			break
		}
	}
	mu.Lock()
	old := states[raw.Item.ID]
	if st.previousItemID == "" {
		st.previousItemID = old.previousItemID
	}
	if st.transcript == "" {
		st.transcript = old.transcript
	}
states[raw.Item.ID] = st
	mu.Unlock()
}

// HACK1: post-response.done truncate workaround. Undo: delete assistant audio capture/transcription repair.
func storeAssistantAudio(states map[string]assistantAudioState, mu *sync.Mutex, itemID string, contentIndex int, data string) {
	if itemID == "" || data == "" {
		return
	}
	pcm, err := base64ToPCM16(data)
	if err != nil {
		log.Printf("store assistant audio: %v", err)
		return
	}
	mu.Lock()
	st := states[itemID]
	if st.contentIndex != contentIndex {
		st = assistantAudioState{contentIndex: contentIndex}
	}
	st.pcm24 = append(st.pcm24, pcm...)
	states[itemID] = st
	mu.Unlock()
}

// HACK1: post-response.done truncate workaround. Undo: delete this function and the DeleteConversationItem/CreateAssistantTextItem methods it calls.
func maybeReplaceCompletedAssistantItem(ctx context.Context, session *voicethread.VoiceSession, writeEvent func(voicethread.Event), states map[string]assistantItemState, mu *sync.Mutex, audioStates map[string]assistantAudioState, audioMu *sync.Mutex, itemID string, contentIndex int, measuredAudioEndMS int) {
	mu.Lock()
	st := states[itemID]
	mu.Unlock()
	replacement := transcribeAssistantPrefix(ctx, audioStates, audioMu, itemID, contentIndex, measuredAudioEndMS)
	if replacement == "" {
		replacement = transcriptPrefixForMS(st.transcript, measuredAudioEndMS)
	}
	if replacement == "" {
		return
	}
	log.Printf("interrupt transcript repair: delete+insert item_id=%q previous_item_id=%q status=%q measured_audio_end_ms=%d replacement=%q", itemID, st.previousItemID, st.status, measuredAudioEndMS, replacement)
	if err := session.DeleteConversationItem(ctx, itemID); err != nil {
		writeEvent(voicethread.Event{Type: "error", Message: "delete completed assistant item: " + err.Error()})
		return
	}
	if err := session.CreateAssistantTextItem(ctx, st.previousItemID, replacement); err != nil {
		writeEvent(voicethread.Event{Type: "error", Message: "insert truncated assistant item: " + err.Error()})
		return
	}
}

// HACK1: post-response.done truncate workaround. Undo: delete transcribeAssistantPrefix/transcribePCM24/writeWAVPCM16 and fallback to normal truncate.
func transcribeAssistantPrefix(ctx context.Context, states map[string]assistantAudioState, mu *sync.Mutex, itemID string, contentIndex int, audioEndMS int) string {
	if audioEndMS <= 0 {
		return ""
	}
	mu.Lock()
	st := states[itemID]
	pcm := append([]int16(nil), st.pcm24...)
	mu.Unlock()
	if st.contentIndex != contentIndex || len(pcm) == 0 {
		return ""
	}
	n := audioEndMS * 24000 / 1000
	if n > len(pcm) {
		n = len(pcm)
	}
	if n <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	text, err := transcribePCM24(ctx, pcm[:n])
	if err != nil {
		log.Printf("transcribe assistant prefix: %v", err)
		return ""
	}
	text = strings.TrimSpace(text)
	if text != "" {
		log.Printf("transcribed assistant prefix item_id=%q audio_end_ms=%d text=%q", itemID, audioEndMS, text)
	}
	return text
}

// HACK1: post-response.done truncate workaround. Undo: remove OpenAI audio transcription call used only for completed-item replacement.
func transcribePCM24(ctx context.Context, pcm []int16) (string, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is not set")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", envDefault("OPENAI_TRANSCRIBE_MODEL", "gpt-4o-mini-transcribe"))
	_ = mw.WriteField("language", "en")
	fw, err := mw.CreateFormFile("file", "assistant-prefix.wav")
	if err != nil {
		return "", err
	}
	if err := writeWAVPCM16(fw, pcm, 24000); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("transcription status %s: %s", resp.Status, string(respBody))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// HACK1: post-response.done truncate workaround. Undo: remove WAV encoder used only for assistant-prefix transcription.
func writeWAVPCM16(w io.Writer, pcm []int16, sampleRate int) error {
	dataLen := uint32(len(pcm) * 2)
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(36)+dataLen); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WAVEfmt ")); err != nil {
		return err
	}
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(sampleRate), uint32(sampleRate * 2), uint16(2), uint16(16)} {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, dataLen); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, pcm)
}

// HACK1: coarse transcript reconstruction for delete+insert workaround. Undo: delete this function when replacing with true audio/transcript timing or removing workaround.
func transcriptPrefixForMS(transcript string, audioEndMS int) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "[Assistant response was interrupted.]"
	}
	if audioEndMS <= 0 {
		return "[Assistant response was interrupted before saying anything further.]"
	}
	// Coarse fallback until we have real word/audio timestamps. Marin counting
	// runs around 10-14 transcript chars/sec in our tests; bias short so future
	// turns do not condition on unheard content.
	n := audioEndMS * 11 / 1000
	if n <= 0 {
		return "[Assistant response was interrupted.]"
	}
	if n >= len(transcript) {
		return transcript
	}
	cut := n
	for cut > 0 && !unicode.IsSpace(rune(transcript[cut-1])) && !strings.ContainsRune(",.;:!?\n", rune(transcript[cut-1])) {
		cut--
	}
	if cut < n/2 {
		cut = n
	}
	return strings.TrimSpace(transcript[:cut])
}

type outputAudioChunk struct {
	data         string
	itemID       string
	contentIndex int
}

type outputPlayhead struct {
	itemID       string
	contentIndex int
	writtenMS    int
	firstWrite   time.Time
	lastWrite    time.Time
}

type encodedOutputFrame struct {
	data         []byte
	pcm24        []int16
	itemID       string
	contentIndex int
	audioEndMS   int
	duration     time.Duration
}

type outputStopResult struct {
	itemID       string
	contentIndex int
	audioEndMS   int
	ok           bool
}

type audioBridge struct {
	browserIn chan []int16
	openAIOut chan outputAudioChunk
	clearOut  chan struct{}
	stopOut   chan chan outputStopResult
	closed    chan struct{}

	mu       sync.RWMutex
	outTrack *webrtc.TrackLocalStaticSample
	playhead outputPlayhead
	dropOut  bool
	// Recent assistant audio at 24 kHz, recorded at the point we actually write
	// frames to the browser. This is only a cheap echo-reference gate for local
	// auto-interrupts; browser WebRTC AEC is still the primary echo canceller.
	recentOutputPCM24 []int16

	outputCodec string
}

func newAudioBridge(outputCodec string) *audioBridge {
	return &audioBridge{
		browserIn: make(chan []int16, 256),
		openAIOut: make(chan outputAudioChunk, 128),
		clearOut:  make(chan struct{}, 8),
		stopOut:     make(chan chan outputStopResult, 8),
		closed:      make(chan struct{}),
		outputCodec: outputCodec,
	}
}

func (b *audioBridge) SetOutputTrack(track *webrtc.TrackLocalStaticSample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outTrack = track
}

func (b *audioBridge) Close() {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
}

func (b *audioBridge) EnqueueOpenAIAudio(chunk outputAudioChunk) {
	b.mu.RLock()
	drop := b.dropOut
	b.mu.RUnlock()
	if drop {
		log.Printf("dropping assistant audio chunk after interrupt")
		return
	}
	select {
	case b.openAIOut <- chunk:
	default:
		log.Printf("dropping assistant audio chunk: outbound queue full")
	}
}

func (b *audioBridge) ClearOutput() {
	b.mu.Lock()
	b.dropOut = true
	b.mu.Unlock()
	select {
	case b.clearOut <- struct{}{}:
	default:
	}
}

func (b *audioBridge) StopOutput(ctx context.Context) outputStopResult {
	b.mu.Lock()
	b.dropOut = true
	b.mu.Unlock()

	reply := make(chan outputStopResult, 1)
	select {
	case b.stopOut <- reply:
	case <-ctx.Done():
		return b.outputStopResult()
	case <-b.closed:
		return b.outputStopResult()
	}
	select {
	case res := <-reply:
		return res
	case <-ctx.Done():
		return b.outputStopResult()
	case <-b.closed:
		return b.outputStopResult()
	}
}

func (b *audioBridge) ResumeOutput() {
	b.mu.Lock()
	b.dropOut = false
	b.mu.Unlock()
}

func (b *audioBridge) EstimatedHeard() (itemID string, contentIndex int, audioEndMS int, ok bool) {
	res := b.outputStopResult()
	return res.itemID, res.contentIndex, res.audioEndMS, res.ok
}

func (b *audioBridge) AssistantRecentlyEmitting() bool {
	b.mu.RLock()
	ph := b.playhead
	drop := b.dropOut
	b.mu.RUnlock()
	return !drop && !ph.lastWrite.IsZero() && time.Since(ph.lastWrite) <= time.Duration(envInt("LOCAL_TURN_ASSISTANT_ACTIVE_MS", 250))*time.Millisecond
}

func (b *audioBridge) MicLikelyEcho(pcm24 []int16) (bool, string) {
	if envInt("LOCAL_TURN_ECHO_SUPPRESS", 1) == 0 || len(pcm24) < 120 {
		return false, ""
	}
	b.mu.RLock()
	ref := append([]int16(nil), b.recentOutputPCM24...)
	lastWrite := b.playhead.lastWrite
	b.mu.RUnlock()
	if len(ref) < len(pcm24) || lastWrite.IsZero() ||
		time.Since(lastWrite) > time.Duration(envInt("LOCAL_TURN_ECHO_REF_ACTIVE_MS", 700))*time.Millisecond {
		return false, ""
	}
	micRMS := pcmRMS(pcm24)
	if micRMS < envFloat("LOCAL_TURN_ECHO_MIC_MIN_RMS", 0.002) {
		return true, fmt.Sprintf("local_turn.interrupt_suppressed_echo mic_rms=%.4f reason=quiet", micRMS)
	}
	bestCorr, bestRefRMS := bestAbsCorrelation(pcm24, ref, envFloat("LOCAL_TURN_ECHO_REF_MIN_RMS", 0.002))
	threshold := envFloat("LOCAL_TURN_ECHO_CORR_THRESHOLD", 0.68)
	if bestCorr >= threshold {
		return true, fmt.Sprintf("local_turn.interrupt_suppressed_echo corr=%.2f threshold=%.2f mic_rms=%.4f ref_rms=%.4f", bestCorr, threshold, micRMS, bestRefRMS)
	}
	return false, fmt.Sprintf("echo_check corr=%.2f threshold=%.2f mic_rms=%.4f ref_rms=%.4f", bestCorr, threshold, micRMS, bestRefRMS)
}

func (b *audioBridge) outputStopResult() outputStopResult {
	b.mu.RLock()
	ph := b.playhead
	b.mu.RUnlock()
	if ph.itemID == "" || ph.firstWrite.IsZero() || ph.writtenMS <= 0 {
		return outputStopResult{}
	}
	// We use the sent-through playhead, not a guessed speaker playhead.
	// Already-written WebRTC frames should still drain in the browser after we
	// stop sending, so the final heard point should be close to the final frame
	// we let through.
	return outputStopResult{itemID: ph.itemID, contentIndex: ph.contentIndex, audioEndMS: ph.writtenMS, ok: true}
}

func (b *audioBridge) resetOutputPlayhead() {
	b.mu.Lock()
	b.playhead = outputPlayhead{}
	b.recentOutputPCM24 = b.recentOutputPCM24[:0]
	b.mu.Unlock()
}

func (b *audioBridge) noteOutputFrame(itemID string, contentIndex int, audioEndMS int, pcm24 []int16) {
	if itemID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.playhead.itemID != itemID || b.playhead.contentIndex != contentIndex {
		b.playhead = outputPlayhead{
			itemID:       itemID,
			contentIndex: contentIndex,
			firstWrite:   now,
		}
	}
	b.playhead.writtenMS = audioEndMS
	b.playhead.lastWrite = now
	if len(pcm24) > 0 {
		b.recentOutputPCM24 = append(b.recentOutputPCM24, pcm24...)
		maxSamples := envInt("LOCAL_TURN_ECHO_REF_MS", 1000) * 24
		if maxSamples <= 0 {
			maxSamples = 24000
		}
		if len(b.recentOutputPCM24) > maxSamples {
			copy(b.recentOutputPCM24, b.recentOutputPCM24[len(b.recentOutputPCM24)-maxSamples:])
			b.recentOutputPCM24 = b.recentOutputPCM24[:maxSamples]
		}
	}
}

// HACK1: post-response.done truncate workaround. Undo: remove itemStates/assistantAudio params.
func (b *audioBridge) Start(ctx context.Context, session *voicethread.VoiceSession, emit func(voicethread.Event), itemStates map[string]assistantItemState, itemStatesMu *sync.Mutex, assistantAudio map[string]assistantAudioState, assistantAudioMu *sync.Mutex) {
	go b.writeOpenAIInputLoop(ctx, session, emit, itemStates, itemStatesMu, assistantAudio, assistantAudioMu)
	go b.writeBrowserOutputLoop(ctx, emit)
}

func (b *audioBridge) ReadBrowserAudio(ctx context.Context, track *webrtc.TrackRemote) {
	if track.Codec().MimeType == webrtc.MimeTypePCMU {
		b.readBrowserPCMU(ctx, track)
		return
	}
	b.readBrowserOpus(ctx, track)
}

func (b *audioBridge) readBrowserOpus(ctx context.Context, track *webrtc.TrackRemote) {
	dec, err := pionopus.NewDecoderWithOutput(48000, 1)
	if err != nil {
		log.Printf("opus decoder: %v", err)
		return
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if err != io.EOF {
				log.Printf("read browser RTP: %v", err)
			}
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		pcm48 := make([]int16, 5760) // 120ms max Opus frame at 48k mono.
		n, err := dec.DecodeToInt16(pkt.Payload, pcm48)
		if err != nil {
			log.Printf("decode browser opus: %v", err)
			continue
		}
		pcm24 := downsample48To24(pcm48[:n])
		if len(pcm24) == 0 {
			continue
		}
		select {
		case b.browserIn <- pcm24:
		case <-ctx.Done():
			return
		case <-b.closed:
			return
		}
	}
}

func (b *audioBridge) readBrowserPCMU(ctx context.Context, track *webrtc.TrackRemote) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if err != io.EOF {
				log.Printf("read browser RTP: %v", err)
			}
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		pcm8 := make([]int16, len(pkt.Payload))
		for i, v := range pkt.Payload {
			pcm8[i] = muLawToLinear(v)
		}
		pcm24 := upsample8To24(pcm8)
		select {
		case b.browserIn <- pcm24:
		case <-ctx.Done():
			return
		case <-b.closed:
			return
		}
	}
}

type localTurnDetector struct {
	smart          *smartturn.Detector
	vad            *smartturn.SileroVAD
	threshold      float64
	speechStartMS  int
	checkpoints    []turnCheckpoint
	inSpeech       bool
	currentlySpeech bool
	committed      bool
	speechMS       int
	silenceMS      int
	nextCheckpoint int
	vadPCM16       []int16
	turnPCM16      []int16
	lastSpeechLog  time.Time
	lastStopAt      time.Time
	interruptSpeechMS int
}

type turnCheckpoint struct {
	silenceMS int
	threshold float64
}

type localTurnDecision struct {
	stop      bool
	interrupt bool
	message   string
}

func newLocalTurnDetector() (*localTurnDetector, error) {
	modelPath := envDefault("SMART_TURN_MODEL", "voicethread/models/smart-turn-v3/smart-turn-v3.2-cpu.onnx")
	det, err := smartturn.NewDetector(smartturn.DetectorConfig{ModelPath: modelPath, Threads: envInt("SMART_TURN_THREADS", 1)})
	if err != nil {
		return nil, err
	}
	vad, err := smartturn.NewSileroVAD(smartturn.SileroVADConfig{Threads: envInt("SMART_TURN_THREADS", 1)})
	if err != nil {
		det.Close()
		return nil, err
	}
	return &localTurnDetector{
		smart:         det,
		vad:           vad,
		threshold:     envFloat("LOCAL_VAD_THRESHOLD", 0.5),
		speechStartMS: envInt("LOCAL_VAD_SPEECH_START_MS", 80),
		checkpoints: []turnCheckpoint{
			{silenceMS: envInt("SMART_TURN_CHECK_1_MS", 250), threshold: envFloat("SMART_TURN_CHECK_1_THRESHOLD", 0.98250)},
			{silenceMS: envInt("SMART_TURN_CHECK_2_MS", 500), threshold: envFloat("SMART_TURN_CHECK_2_THRESHOLD", 0.970)},
			{silenceMS: envInt("SMART_TURN_CHECK_3_MS", 800), threshold: envFloat("SMART_TURN_CHECK_3_THRESHOLD", 0.900)},
			{silenceMS: envInt("SMART_TURN_CHECK_4_MS", 1000), threshold: envFloat("SMART_TURN_CHECK_4_THRESHOLD", 0.850)},
			{silenceMS: envInt("SMART_TURN_CHECK_5_MS", 2000), threshold: envFloat("SMART_TURN_CHECK_5_THRESHOLD", 0.940)},
			{silenceMS: envInt("SMART_TURN_CHECK_6_MS", 3000), threshold: envFloat("SMART_TURN_CHECK_6_THRESHOLD", 0.920)},
			{silenceMS: envInt("SMART_TURN_CHECK_7_MS", 4500), threshold: envFloat("SMART_TURN_CHECK_7_THRESHOLD", 0.900)},
		},
	}, nil
}

func (d *localTurnDetector) Close() {
	if d != nil && d.smart != nil {
		d.smart.Close()
	}
	if d != nil && d.vad != nil {
		d.vad.Close()
	}
}

func (d *localTurnDetector) Observe(ctx context.Context, pcm24 []int16, assistantActive bool, suppressInterrupt bool, emit func(voicethread.Event)) localTurnDecision {
	if d == nil || len(pcm24) == 0 {
		return localTurnDecision{}
	}
	d.vadPCM16 = append(d.vadPCM16, downsample24To16(pcm24)...)
	var decision localTurnDecision
	for len(d.vadPCM16) >= smartturn.SileroChunkSamples {
		chunk := d.vadPCM16[:smartturn.SileroChunkSamples]
		if dec := d.observeVADChunk(ctx, chunk, assistantActive, suppressInterrupt, emit); dec.stop || dec.interrupt {
			decision = dec
		}
		copy(d.vadPCM16, d.vadPCM16[smartturn.SileroChunkSamples:])
		d.vadPCM16 = d.vadPCM16[:len(d.vadPCM16)-smartturn.SileroChunkSamples]
	}
	return decision
}

func (d *localTurnDetector) observeVADChunk(ctx context.Context, pcm16 []int16, assistantActive bool, suppressInterrupt bool, emit func(voicethread.Event)) localTurnDecision {
	const chunkMS = smartturn.SileroChunkSamples * 1000 / smartturn.SampleRate
	// Cheap wake gate: avoid running Silero continuously on room silence. While
	// already inside a turn, always run Silero so trailing silence is measured
	// by the model. Outside a turn, only call Silero if there is enough raw
	// energy that speech might be starting.
	if !d.inSpeech && pcmRMS(pcm16) < envFloat("LOCAL_VAD_WAKE_RMS", 0.003) {
		return localTurnDecision{}
	}
	prob, _, err := d.vad.ProbabilityPCM16(ctx, pcm16, smartturn.SampleRate)
	if err != nil {
		emit(voicethread.Event{Type: "error", Message: "silero vad: " + err.Error()})
		return localTurnDecision{}
	}
	isSpeech := float64(prob) >= d.threshold
	if isSpeech {
		var decision localTurnDecision
		if d.silenceMS > 0 {
			d.nextCheckpoint = 0
		}
		d.turnPCM16 = append(d.turnPCM16, pcm16...)
		d.speechMS += chunkMS
		d.silenceMS = 0
		if assistantActive && !suppressInterrupt && float64(prob) >= envFloat("LOCAL_TURN_INTERRUPT_VAD_THRESHOLD", 0.98) {
			d.interruptSpeechMS += chunkMS
			sustainMS := envInt("LOCAL_TURN_INTERRUPT_SUSTAIN_MS", 250)
			elapsedMS := int64(-1)
			if !d.lastStopAt.IsZero() {
				elapsedMS = time.Since(d.lastStopAt).Milliseconds()
			}
			if d.interruptSpeechMS >= sustainMS {
				msg := fmt.Sprintf("local_turn.interrupt_after_stop elapsed_ms=%d p=%.3f sustain_ms=%d", elapsedMS, prob, d.interruptSpeechMS)
				log.Print(msg)
				decision = localTurnDecision{interrupt: true, message: msg}
				d.interruptSpeechMS = 0
			} else if d.interruptSpeechMS == chunkMS {
				msg := fmt.Sprintf("local_turn.interrupt_candidate elapsed_ms=%d p=%.3f sustain_ms=%d/%d", elapsedMS, prob, d.interruptSpeechMS, sustainMS)
				log.Print(msg)
				emit(voicethread.Event{Type: "debug", Message: msg})
			}
		} else {
			d.interruptSpeechMS = 0
		}
		if !d.currentlySpeech && d.speechMS >= d.speechStartMS {
			d.currentlySpeech = true
			d.inSpeech = true
			d.committed = false
			d.nextCheckpoint = 0
			msg := fmt.Sprintf("local_vad.speech_started p=%.3f", prob)
			log.Print(msg)
			emit(voicethread.Event{Type: "debug", Message: msg})
		}
		return decision
	}

	d.speechMS = 0
	d.interruptSpeechMS = 0
	if !d.inSpeech {
		return localTurnDecision{}
	}
	if d.currentlySpeech {
		d.currentlySpeech = false
		msg := fmt.Sprintf("local_vad.speech_stopped p=%.3f", prob)
		log.Print(msg)
		emit(voicethread.Event{Type: "debug", Message: msg})
	}
	d.turnPCM16 = append(d.turnPCM16, pcm16...)
	d.silenceMS += chunkMS
	if d.committed || d.nextCheckpoint >= len(d.checkpoints) {
		return localTurnDecision{}
	}
	cp := d.checkpoints[d.nextCheckpoint]
	if d.silenceMS < cp.silenceMS {
		return localTurnDecision{}
	}
	d.nextCheckpoint++
	start := time.Now()
	result, err := d.smart.PredictPCM16(ctx, d.turnPCM16, smartturn.SampleRate)
	if err != nil {
		emit(voicethread.Event{Type: "error", Message: "smartturn: " + err.Error()})
		return localTurnDecision{}
	}
	totalAudioMS := len(d.turnPCM16) * 1000 / smartturn.SampleRate
	msg := fmt.Sprintf("smartturn silence_ms=%d audio_ms=%d p=%.3f threshold=%.2f complete=%v inference_ms=%d wall_ms=%d",
		d.silenceMS, totalAudioMS, result.Probability, cp.threshold, result.Probability >= float32(cp.threshold), result.Duration.Milliseconds(), time.Since(start).Milliseconds())
	log.Print(msg)
	emit(voicethread.Event{Type: "debug", Message: msg})
	if result.Probability >= float32(cp.threshold) {
		d.lastStopAt = time.Now()
		emit(voicethread.Event{Type: "local.turn.stop", Message: msg})
		emit(voicethread.Event{Type: "debug", Message: "smartturn.detected_stop"})
		d.resetTurn()
		return localTurnDecision{stop: true, message: msg}
	}
	return localTurnDecision{}
}

func (d *localTurnDetector) resetTurn() {
	d.inSpeech = false
	d.currentlySpeech = false
	d.committed = false
	d.speechMS = 0
	d.silenceMS = 0
	d.nextCheckpoint = 0
	d.turnPCM16 = d.turnPCM16[:0]
	d.interruptSpeechMS = 0
}

// HACK1: post-response.done truncate workaround. Undo: remove itemStates/assistantAudio params and interruptAssistant call.
func (b *audioBridge) writeOpenAIInputLoop(ctx context.Context, session *voicethread.VoiceSession, emit func(voicethread.Event), itemStates map[string]assistantItemState, itemStatesMu *sync.Mutex, assistantAudio map[string]assistantAudioState, assistantAudioMu *sync.Mutex) {
	det, err := newLocalTurnDetector()
	if err != nil {
		emit(voicethread.Event{Type: "error", Message: "smartturn init: " + err.Error()})
		return
	}
	defer det.Close()
	emit(voicethread.Event{Type: "debug", Message: "smartturn local detection enabled; sending mic audio to OpenAI"})
	for {
		select {
		case pcm := <-b.browserIn:
			assistantActive := b.AssistantRecentlyEmitting()
			suppressInterrupt := false
			if assistantActive {
				if echo, msg := b.MicLikelyEcho(pcm); echo {
					suppressInterrupt = true
					log.Print(msg)
					emit(voicethread.Event{Type: "debug", Message: msg})
				}
			}
			if err := session.AppendAudio(ctx, pcm16Base64(pcm)); err != nil {
				emit(voicethread.Event{Type: "error", Message: "append browser audio: " + err.Error()})
				return
			}
			decision := det.Observe(ctx, pcm, assistantActive, suppressInterrupt, emit)
			if decision.interrupt && assistantActive {
				if echo, msg := b.MicLikelyEcho(pcm); echo {
					log.Print(msg)
					emit(voicethread.Event{Type: "debug", Message: msg})
				} else {
					if msg != "" {
						emit(voicethread.Event{Type: "debug", Message: msg})
					}
					emit(voicethread.Event{Type: "local.turn.interrupt", Message: decision.message})
					interruptAssistant(ctx, session, b, emit, itemStates, itemStatesMu, assistantAudio, assistantAudioMu)
				}
			}
			if decision.stop {
				if err := session.CommitAudio(ctx); err != nil {
					emit(voicethread.Event{Type: "error", Message: "smartturn commit: " + err.Error()})
				} else {
					emit(voicethread.Event{Type: "debug", Message: "smartturn committed audio"})
				}
			}
		case <-ctx.Done():
			return
		case <-b.closed:
			return
		}
	}
}

func (b *audioBridge) writeBrowserOutputLoop(ctx context.Context, emit func(voicethread.Event)) {
	if b.outputCodec == "opus" {
		b.writeBrowserOpusOutputLoop(ctx, emit)
		return
	}
	b.writeBrowserPCMUOutputLoop(ctx, emit)
}

func (b *audioBridge) writeBrowserOpusOutputLoop(ctx context.Context, emit func(voicethread.Event)) {
	var enc *kopus.Encoder
	var err error
	// Match opus-go's wav2oggopus example settings. ApplicationVoIP at 32kbps
	// round-tripped as garbage with this encoder; ApplicationAudio/64kbps
	// passes the offline similarity check.
	enc, err = kopus.NewEncoder(48000, 2, kopus.ApplicationAudio)
	if err != nil {
		emit(voicethread.Event{Type: "error", Message: "opus encoder: " + err.Error()})
		return
	}
	defer enc.Close()
	_ = enc.SetBitrate(64000)
	_ = enc.SetVBR(true)
	_ = enc.SetComplexity(10)

	rs, err := resampling.New(&resampling.Config{
		InputRate:  24000,
		OutputRate: 48000,
		Channels:   1,
		Quality:    resampling.QualitySpec{Preset: resampling.QualityMedium},
	})
	if err != nil {
		emit(voicethread.Event{Type: "error", Message: "resampler: " + err.Error()})
		return
	}

	var pcm48Buf []int16
	var encodedQ []encodedOutputFrame
	var nextFrameAt time.Time
	var active bool
	var silenceFrames int
	var currentItemID string
	var currentContentIndex int
	var sourceAudioMS int
	silentStereo := make([]int16, 960*2)
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}
	defer stopTimer()

	processChunk := func(chunk outputAudioChunk) bool {
		if chunk.itemID != currentItemID || chunk.contentIndex != currentContentIndex {
			currentItemID = chunk.itemID
			currentContentIndex = chunk.contentIndex
			sourceAudioMS = 0
		}
		if !active && len(encodedQ) == 0 && len(pcm48Buf) == 0 {
			packet, err := encodeOpusPacket(enc, silentStereo)
			if err != nil {
				emit(voicethread.Event{Type: "error", Message: "encode leading silence: " + err.Error()})
				return false
			}
			if err := b.writeEncodedFrame(packet, 20*time.Millisecond); err != nil {
				emit(voicethread.Event{Type: "error", Message: "write leading silence: " + err.Error()})
				return false
			}
			active = true
			nextFrameAt = time.Now()
			silenceFrames = 0
		}
		pcm24, err := base64ToPCM16(chunk.data)
		if err != nil {
			emit(voicethread.Event{Type: "error", Message: "decode OpenAI audio: " + err.Error()})
			return true
		}
		pcm48, err := rs.Process(int16ToFloat64(pcm24))
		if err != nil {
			emit(voicethread.Event{Type: "error", Message: "resample OpenAI audio: " + err.Error()})
			return false
		}
		pcm48Buf = append(pcm48Buf, float64ToInt16Scaled(pcm48, 0.85)...)
		for len(pcm48Buf) >= 960 {
			frame48 := pcm48Buf[:960]
			pcm48Buf = pcm48Buf[960:]
			packet, err := encodeOpusPacket(enc, monoToStereo(frame48))
			if err != nil {
				emit(voicethread.Event{Type: "error", Message: "encode browser audio: " + err.Error()})
				return false
			}
			encodedQ = append(encodedQ, encodedOutputFrame{
				data:         packet,
				pcm24:        downsample48To24(frame48),
				itemID:       chunk.itemID,
				contentIndex: chunk.contentIndex,
				audioEndMS:   sourceAudioMS + 20,
				duration:     20 * time.Millisecond,
			})
			sourceAudioMS += 20
		}
		return true
	}
	clear := func() {
		pcm48Buf = pcm48Buf[:0]
		encodedQ = encodedQ[:0]
		nextFrameAt = time.Time{}
		active = false
		silenceFrames = 0
		currentItemID = ""
		currentContentIndex = 0
		sourceAudioMS = 0
		rs.Reset()
		stopTimer()
		drainAudioQueue(b.openAIOut)
		b.resetOutputPlayhead()
	}

	for {
		select {
		case reply := <-b.stopOut:
			res := b.outputStopResult()
			clear()
			reply <- res
			continue
		case <-b.clearOut:
			clear()
			continue
		default:
		}
		if !active && len(encodedQ) > 0 {
			active = true
			nextFrameAt = time.Time{}
		}
		if active && timerC == nil {
			now := time.Now()
			if nextFrameAt.IsZero() || nextFrameAt.Before(now.Add(-200*time.Millisecond)) {
				nextFrameAt = now
			}
			nextFrameAt = nextFrameAt.Add(20 * time.Millisecond)
			wait := time.Until(nextFrameAt)
			if wait < 0 {
				nextFrameAt = now
				wait = 0
			}
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case chunk := <-b.openAIOut:
			if !processChunk(chunk) {
				return
			}
		drainInput:
			for {
				select {
				case chunk := <-b.openAIOut:
					if !processChunk(chunk) {
						return
					}
				case <-b.clearOut:
					clear()
					break drainInput
				case reply := <-b.stopOut:
					res := b.outputStopResult()
					clear()
					reply <- res
					break drainInput
				default:
					break drainInput
				}
			}
		case <-timerC:
			timer = nil
			timerC = nil
			if len(encodedQ) == 0 {
				const maxGapFillFrames = 3
				if silenceFrames >= maxGapFillFrames {
					active = false
					nextFrameAt = time.Time{}
					silenceFrames = 0
					continue
				}
				packet, err := encodeOpusPacket(enc, silentStereo)
				if err != nil {
					emit(voicethread.Event{Type: "error", Message: "encode silence: " + err.Error()})
					return
				}
				if err := b.writeEncodedFrame(packet, 20*time.Millisecond); err != nil {
					emit(voicethread.Event{Type: "error", Message: "write browser silence: " + err.Error()})
					return
				}
				silenceFrames++
				continue
			}
			frame := encodedQ[0]
			copy(encodedQ, encodedQ[1:])
			encodedQ = encodedQ[:len(encodedQ)-1]
			if err := b.writeEncodedFrame(frame.data, frame.duration); err != nil {
				emit(voicethread.Event{Type: "error", Message: "write browser audio: " + err.Error()})
				return
			}
			b.noteOutputFrame(frame.itemID, frame.contentIndex, frame.audioEndMS, frame.pcm24)
			if frame.itemID != "" {
				silenceFrames = 0
			}
		case <-b.clearOut:
			clear()
		case reply := <-b.stopOut:
			res := b.outputStopResult()
			clear()
			reply <- res
		case <-ctx.Done():
			return
		case <-b.closed:
			return
		}
	}
}

func (b *audioBridge) writeBrowserPCMUOutputLoop(ctx context.Context, emit func(voicethread.Event)) {
	var pcm24Buf []int16
	var encodedQ []encodedOutputFrame
	var nextFrameAt time.Time
	var active bool
	var currentItemID string
	var currentContentIndex int
	var sourceAudioMS int
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		timerC = nil
	}
	defer stopTimer()

	processChunk := func(chunk outputAudioChunk) bool {
		if chunk.itemID != currentItemID || chunk.contentIndex != currentContentIndex {
			currentItemID = chunk.itemID
			currentContentIndex = chunk.contentIndex
			sourceAudioMS = 0
		}
		pcm24, err := base64ToPCM16(chunk.data)
		if err != nil {
			emit(voicethread.Event{Type: "error", Message: "decode OpenAI audio: " + err.Error()})
			return true
		}
		pcm24Buf = append(pcm24Buf, pcm24...)
		for len(pcm24Buf) >= 480 {
			frame24 := pcm24Buf[:480]
			pcm24Buf = pcm24Buf[480:]
			encodedQ = append(encodedQ, encodedOutputFrame{
				data:         pcmuBytes(downsample24To8(frame24)),
				pcm24:        append([]int16(nil), frame24...),
				itemID:       chunk.itemID,
				contentIndex: chunk.contentIndex,
				audioEndMS:   sourceAudioMS + 20,
				duration:     20 * time.Millisecond,
			})
			sourceAudioMS += 20
		}
		return true
	}
	clear := func() {
		pcm24Buf = pcm24Buf[:0]
		encodedQ = encodedQ[:0]
		nextFrameAt = time.Time{}
		active = false
		currentItemID = ""
		currentContentIndex = 0
		sourceAudioMS = 0
		stopTimer()
		drainAudioQueue(b.openAIOut)
		b.resetOutputPlayhead()
	}

	for {
		select {
		case reply := <-b.stopOut:
			res := b.outputStopResult()
			clear()
			reply <- res
			continue
		case <-b.clearOut:
			clear()
			continue
		default:
		}
		if !active && len(encodedQ) > 0 {
			active = true
			nextFrameAt = time.Time{}
		}
		if active && timerC == nil {
			now := time.Now()
			if nextFrameAt.IsZero() || nextFrameAt.Before(now.Add(-200*time.Millisecond)) {
				nextFrameAt = now
			}
			nextFrameAt = nextFrameAt.Add(20 * time.Millisecond)
			wait := time.Until(nextFrameAt)
			if wait < 0 {
				nextFrameAt = now
				wait = 0
			}
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		select {
		case chunk := <-b.openAIOut:
			if !processChunk(chunk) {
				return
			}
		drainInput:
			for {
				select {
				case chunk := <-b.openAIOut:
					if !processChunk(chunk) {
						return
					}
				case <-b.clearOut:
					clear()
					break drainInput
				case reply := <-b.stopOut:
					res := b.outputStopResult()
					clear()
					reply <- res
					break drainInput
				default:
					break drainInput
				}
			}
		case <-timerC:
			timer = nil
			timerC = nil
			if len(encodedQ) == 0 {
				active = false
				nextFrameAt = time.Time{}
				continue
			}
			frame := encodedQ[0]
			copy(encodedQ, encodedQ[1:])
			encodedQ = encodedQ[:len(encodedQ)-1]
			if err := b.writeEncodedFrame(frame.data, frame.duration); err != nil {
				emit(voicethread.Event{Type: "error", Message: "write browser audio: " + err.Error()})
				return
			}
			b.noteOutputFrame(frame.itemID, frame.contentIndex, frame.audioEndMS, frame.pcm24)
		case <-b.clearOut:
			clear()
		case reply := <-b.stopOut:
			res := b.outputStopResult()
			clear()
			reply <- res
		case <-ctx.Done():
			return
		case <-b.closed:
			return
		}
	}
}

func encodeOpusPacket(enc *kopus.Encoder, pcm48 []int16) ([]byte, error) {
	if enc == nil {
		return nil, fmt.Errorf("opus encoder is nil")
	}
	buf := make([]byte, 1500)
	n, err := enc.Encode(pcm48, 960, buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}

func (b *audioBridge) writeEncodedFrame(data []byte, dur time.Duration) error {
	b.mu.RLock()
	track := b.outTrack
	b.mu.RUnlock()
	if track == nil {
		return nil
	}
	return track.WriteSample(media.Sample{Data: data, Duration: dur})
}

func monoToStereo(in []int16) []int16 {
	out := make([]int16, len(in)*2)
	for i, v := range in {
		out[i*2] = v
		out[i*2+1] = v
	}
	return out
}

func pcmuBytes(pcm8 []int16) []byte {
	buf := make([]byte, len(pcm8))
	for i, v := range pcm8 {
		buf[i] = linearToMuLaw(v)
	}
	return buf
}

func drainAudioQueue(ch <-chan outputAudioChunk) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func downsample48To24(in []int16) []int16 {
	out := make([]int16, len(in)/2)
	for i := range out {
		out[i] = in[i*2]
	}
	return out
}

func downsample24To8(in []int16) []int16 {
	out := make([]int16, len(in)/3)
	for i := range out {
		out[i] = in[i*3]
	}
	return out
}

func downsample24To16(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int16, len(in)*2/3)
	for i := range out {
		// Linear interpolation at positions 0, 1.5, 3.0, 4.5, ...
		pos2 := i * 3 // position * 2 in input-sample units
		j := pos2 / 2
		if pos2%2 == 0 || j+1 >= len(in) {
			out[i] = in[j]
		} else {
			out[i] = int16((int(in[j]) + int(in[j+1])) / 2)
		}
	}
	return out
}

func pcmRMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		v := float64(s) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

func bestAbsCorrelation(needle, haystack []int16, minRefRMS float64) (bestCorr float64, bestRefRMS float64) {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return 0, 0
	}
	var needleEnergy float64
	for _, s := range needle {
		v := float64(s) / 32768.0
		needleEnergy += v * v
	}
	if needleEnergy <= 0 {
		return 0, 0
	}
	step := len(needle) / 4
	if step < 120 {
		step = 120
	}
	for start := 0; start+len(needle) <= len(haystack); start += step {
		var dot, refEnergy float64
		for i, s := range needle {
			a := float64(s) / 32768.0
			b := float64(haystack[start+i]) / 32768.0
			dot += a * b
			refEnergy += b * b
		}
		if refEnergy <= 0 {
			continue
		}
		refRMS := math.Sqrt(refEnergy / float64(len(needle)))
		if refRMS < minRefRMS {
			continue
		}
		corr := math.Abs(dot) / math.Sqrt(needleEnergy*refEnergy)
		if corr > bestCorr {
			bestCorr = corr
			bestRefRMS = refRMS
		}
	}
	return bestCorr, bestRefRMS
}

func resample24To48(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int16, len(in)*2)
	for i, v := range in {
		out[i*2] = v
		if i+1 < len(in) {
			out[i*2+1] = int16((int(v) + int(in[i+1])) / 2)
		} else {
			out[i*2+1] = v
		}
	}
	return out
}

func upsample8To24(in []int16) []int16 {
	out := make([]int16, len(in)*3)
	for i, v := range in {
		out[i*3] = v
		out[i*3+1] = v
		out[i*3+2] = v
	}
	return out
}

func linearToMuLaw(sample int16) byte {
	const bias = 0x84
	const clip = 32635
	s := int(sample)
	sign := 0
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > clip {
		s = clip
	}
	s += bias
	exponent := 7
	for mask := 0x4000; (s&mask) == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := (s >> (exponent + 3)) & 0x0F
	return byte(^(sign | (exponent << 4) | mantissa))
}

func muLawToLinear(mu byte) int16 {
	mu = ^mu
	sign := mu & 0x80
	exponent := (mu >> 4) & 0x07
	mantissa := mu & 0x0F
	sample := int(((int(mantissa) << 3) + 0x84) << exponent)
	sample -= 0x84
	if sign != 0 {
		sample = -sample
	}
	return int16(sample)
}

func pcm16Base64(pcm []int16) string {
	b := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func base64ToPCM16(s string) ([]int16, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	pcm := make([]int16, len(b)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return pcm, nil
}

func int16ToFloat64(pcm []int16) []float64 {
	out := make([]float64, len(pcm))
	for i, v := range pcm {
		out[i] = float64(v) / 32768.0
	}
	return out
}

func float64ToInt16Scaled(pcm []float64, scale float64) []int16 {
	out := make([]int16, len(pcm))
	for i, v := range pcm {
		v *= scale
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		if v >= 0 {
			out[i] = int16(v * 32767.0)
		} else {
			out[i] = int16(v * 32768.0)
		}
	}
	return out
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func exampleTools() *tool.Catalog {
	catalog := tool.NewCatalog()

	timeSpec, timeHandler := tool.JSON("get_time", "Get the current local time.", func(ctx context.Context, thread *threads.Thread, call tool.Call, args struct{}) tool.Item {
		return tool.ResultText(call, time.Now().Format(time.RFC1123))
	})
	catalog.Add(timeSpec, timeHandler)

	type echoArgs struct {
		Text string `json:"text" jsonschema:"text to echo back"`
	}
	echoSpec, echoHandler := tool.JSON("echo", "Echo text back to the user.", func(ctx context.Context, thread *threads.Thread, call tool.Call, args echoArgs) tool.Item {
		return tool.ResultText(call, fmt.Sprintf("echo: %s", args.Text))
	})
	catalog.Add(echoSpec, echoHandler)

	return catalog
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q: %v", name, v, err)
		return fallback
	}
	return n
}

func envFloat(name string, fallback float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("invalid %s=%q: %v", name, v, err)
		return fallback
	}
	return n
}

func logVoiceEvent(ev voicethread.Event) {
	data := ev.Data
	if data != "" {
		data = fmt.Sprintf("<%d base64 chars>", len(data))
	}
	raw := string(ev.Raw)
	if raw != "" {
		raw = truncate(raw, 1200)
	}
	contentIndex := ""
	if ev.ContentIndex != nil {
		contentIndex = fmt.Sprint(*ev.ContentIndex)
	}
	log.Printf("voice event type=%q text=%q data=%q name=%q arguments=%q output=%q message=%q item_id=%q content_index=%q audio_end_ms=%d response_id=%q raw=%s",
		ev.Type,
		truncate(ev.Text, 300),
		data,
		ev.Name,
		truncate(ev.Arguments, 500),
		truncate(ev.Output, 500),
		truncate(ev.Message, 500),
		ev.ItemID,
		contentIndex,
		ev.AudioEndMS,
		ev.ResponseID,
		raw,
	)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
