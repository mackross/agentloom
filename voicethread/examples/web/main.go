package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tool"
	"github.com/mackross/agentloom/voicethread"
)

//go:embed static/*
var staticFS embed.FS

type clientMessage struct {
	Type         string `json:"type"`
	Data         string `json:"data,omitempty"`
	Text         string `json:"text,omitempty"`
	ItemID       string `json:"item_id,omitempty"`
	ContentIndex *int   `json:"content_index,omitempty"`
	AudioEndMS   int    `json:"audio_end_ms,omitempty"`
	ClientTimeMS int64  `json:"client_time_ms,omitempty"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.Handle("/static/", noStore(http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/voice", serveVoice)

	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	log.Printf("voice example listening on http://localhost%s", addr)
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

func serveVoice(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("accept websocket: %v", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()
	var writeMu sync.Mutex
	writeEvent := func(ev voicethread.Event) {
		if ev.ServerTimeMS == 0 {
			ev.ServerTimeMS = time.Now().UnixMilli()
		}
		logVoiceEvent(ev)
		b, err := json.Marshal(ev)
		if err != nil {
			log.Printf("marshal event: %v", err)
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			log.Printf("write browser websocket: %v", err)
		}
	}

	var session *voicethread.VoiceSession
	defer func() {
		if session != nil {
			_ = session.Close()
		}
	}()

	writeEvent(voicethread.Event{Type: "browser.connected"})
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg clientMessage
		if err := json.Unmarshal(b, &msg); err != nil {
			writeEvent(voicethread.Event{Type: "error", Message: "bad browser message: " + err.Error()})
			continue
		}
		switch msg.Type {
		case "start":
			if session != nil {
				writeEvent(voicethread.Event{Type: "debug", Message: "session already started"})
				continue
			}
			apiKey := os.Getenv("OPENAI_API_KEY")
			if apiKey == "" {
				writeEvent(voicethread.Event{Type: "error", Message: "set OPENAI_API_KEY before running the example"})
				continue
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
				writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				_ = session.Close()
				session = nil
			}
		case "audio":
			if session != nil && msg.Data != "" {
				if err := session.AppendAudio(ctx, msg.Data); err != nil {
					writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				}
			}
		case "commit":
			if session != nil {
				if err := session.CommitAudio(ctx); err != nil {
					writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				}
			}
		case "text":
			if session != nil && msg.Text != "" {
				if err := session.SendText(ctx, msg.Text); err != nil {
					writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				}
			}
		case "summary":
			if session != nil {
				if err := session.SummarizeSinceLast(ctx); err != nil {
					writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				}
			}
		case "interrupt":
			if session != nil {
				if msg.ItemID != "" && msg.ContentIndex != nil {
					log.Printf("browser interrupt truncate item_id=%q content_index=%d audio_end_ms=%d", msg.ItemID, *msg.ContentIndex, msg.AudioEndMS)
					if err := session.TruncateAssistantAudio(ctx, msg.ItemID, *msg.ContentIndex, msg.AudioEndMS); err != nil {
						writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
					}
				}
				if err := session.Interrupt(ctx); err != nil {
					writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				}
			}
		case "truncate":
			if session != nil && msg.ItemID != "" && msg.ContentIndex != nil {
				log.Printf("browser truncate item_id=%q content_index=%d audio_end_ms=%d", msg.ItemID, *msg.ContentIndex, msg.AudioEndMS)
				if err := session.TruncateAssistantAudio(ctx, msg.ItemID, *msg.ContentIndex, msg.AudioEndMS); err != nil {
					writeEvent(voicethread.Event{Type: "error", Message: err.Error()})
				}
			}
		case "browser.log":
			log.Printf("browser log: %s", msg.Text)
		case "ping":
			writeEvent(voicethread.Event{
				Type:         "pong",
				ClientTimeMS: msg.ClientTimeMS,
				ServerTimeMS: time.Now().UnixMilli(),
			})
		default:
			writeEvent(voicethread.Event{Type: "error", Message: "unknown browser message type: " + msg.Type})
		}
	}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
