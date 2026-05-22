package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	"github.com/mackross/agentloom/voicethread"
)

//go:embed static/*
var staticFS embed.FS

type sessionRequest struct {
	Voice string `json:"voice,omitempty"`
}

type clientSecretRequest struct {
	Session realtimeSessionConfig `json:"session"`
}

type realtimeSessionConfig struct {
	Type  string `json:"type"`
	Model string `json:"model"`
}

type sidebandRequest struct {
	CallID string `json:"call_id"`
}

var sidebands = struct {
	sync.Mutex
	sessions map[string]*voicethread.VoiceSession
}{sessions: map[string]*voicethread.VoiceSession{}}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.Handle("/static/", noStore(http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/session", serveSession)
	mux.HandleFunc("/sideband", serveSideband)
	mux.HandleFunc("/cancel", serveCancel)
	mux.HandleFunc("/log", serveBrowserLog)

	addr := envDefault("ADDR", ":8080")
	log.Printf("WebRTC sideband voice example listening on http://localhost%s", addr)
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

func serveSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		http.Error(w, "set OPENAI_API_KEY before running the example", http.StatusInternalServerError)
		return
	}
	var req sessionRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Keep the browser's ephemeral session intentionally minimal. The Go sideband
	// connection joins by call_id and sends the real instructions/tools via
	// session.update, so business logic stays server-controlled.
	body := clientSecretRequest{Session: realtimeSessionConfig{
		Type:  "realtime",
		Model: envDefault("OPENAI_REALTIME_MODEL", "gpt-realtime-2"),
	}}
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	reqOpenAI, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/realtime/client_secrets", bytes.NewReader(b))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reqOpenAI.Header.Set("Authorization", "Bearer "+apiKey)
	reqOpenAI.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(reqOpenAI)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("client_secrets failed status=%d body=%s", resp.StatusCode, truncate(string(respBody), 2000))
		http.Error(w, string(respBody), resp.StatusCode)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}

func serveSideband(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		http.Error(w, "set OPENAI_API_KEY before running the example", http.StatusInternalServerError)
		return
	}
	var req sidebandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CallID == "" {
		http.Error(w, "missing call_id", http.StatusBadRequest)
		return
	}
	sidebands.Lock()
	if existing := sidebands.sessions[req.CallID]; existing != nil {
		sidebands.Unlock()
		_ = existing.Close()
	} else {
		sidebands.Unlock()
	}

	session := voicethread.New(voicethread.Options{
		APIKey:                apiKey,
		BaseURL:               "wss://api.openai.com/v1/realtime?call_id=" + url.QueryEscape(req.CallID),
		Model:                 envDefault("OPENAI_REALTIME_MODEL", "gpt-realtime-2"),
		Voice:                 envDefault("OPENAI_REALTIME_VOICE", "marin"),
		TranscriptionLanguage: "en",
		Instructions:          "You are a concise voice assistant. You can use tools for time and echo. Speak naturally and briefly.",
		ToolRuntime:           voicethread.NewCatalogRuntime(exampleTools()),
		OnEvent:               logVoiceEvent,
	})
	if err := session.Start(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	sidebands.Lock()
	sidebands.sessions[req.CallID] = session
	sidebands.Unlock()
	log.Printf("sideband connected call_id=%s", req.CallID)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func serveCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req sidebandRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	sidebands.Lock()
	session := sidebands.sessions[req.CallID]
	sidebands.Unlock()
	if session == nil {
		http.Error(w, "unknown call_id", http.StatusNotFound)
		return
	}
	if err := session.Interrupt(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func serveBrowserLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	log.Printf("browser log: %s", truncate(string(b), 4000))
	w.WriteHeader(http.StatusNoContent)
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
	log.Printf("voice event type=%q text=%q data=%q name=%q arguments=%q output=%q message=%q item_id=%q content_index=%q response_id=%q raw=%s",
		ev.Type,
		truncate(ev.Text, 300),
		data,
		ev.Name,
		truncate(ev.Arguments, 500),
		truncate(ev.Output, 500),
		truncate(ev.Message, 500),
		ev.ItemID,
		contentIndex,
		ev.ResponseID,
		raw,
	)
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
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
