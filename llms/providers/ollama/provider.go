package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/mackross/agentloom/llms"
	"github.com/mackross/agentloom/threads"
)

// ID is the provider id.
const ID = "ollama"

// Provider describes a local or remote Ollama server. Ollama has no curated
// models and cannot claim bare ids; models come from configuration or the
// "ollama/<model>" form. Model.Options keys:
//
//	host        server URL (default OLLAMA_HOST or http://localhost:11434)
//	headers     table of request headers
//	options     table passed as Ollama options (num_ctx, temperature, ...)
//	think       false, true, or "low" | "medium" | "high" | "max"
//	keep_alive  duration string or seconds
//	truncate, shift, allow_best_effort_tool_controls   booleans
var Provider = &llms.Provider{
	ID:   ID,
	Open: open,
}

func open(_ context.Context, m llms.Model, o llms.Options) (threads.LLMStreamer, error) {
	fail := func(key string, err error) (threads.LLMStreamer, error) {
		return nil, fmt.Errorf("ollama %s: %s: %w", m.Name, key, err)
	}
	host := o.BaseURL
	if host == "" {
		host, _ = m.Options["host"].(string)
	}
	var s *ChatStreamer
	if strings.TrimSpace(host) == "" {
		s = NewChatStreamer(m.ID)
		if s.baseURLError != nil {
			return fail("OLLAMA_HOST", s.baseURLError)
		}
		if o.HTTPClient != nil {
			s.client = o.HTTPClient
		}
	} else {
		base, err := parseHost(host)
		if err != nil {
			return fail("host", err)
		}
		s = NewChatStreamerWithClient(o.HTTPClient, base, m.ID)
	}
	for key, raw := range m.Options {
		var err error
		switch key {
		case "host":
		case "headers":
			s.Headers, err = headers(raw)
		case "options":
			s.Options, err = optionsTable(raw)
		case "think":
			err = validateThink(raw)
			s.Think = raw
		case "keep_alive":
			err = validateKeepAlive(raw)
			s.KeepAlive = raw
		case "truncate":
			s.Truncate, err = boolPtr(raw)
		case "shift":
			s.Shift, err = boolPtr(raw)
		case "allow_best_effort_tool_controls":
			var p *bool
			p, err = boolPtr(raw)
			s.AllowBestEffortToolControls = p != nil && *p
		}
		if err != nil {
			return fail(key, err)
		}
	}
	s.defaultThink = s.Think
	return s, nil
}

func parseHost(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid URL %q", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func headers(raw any) (http.Header, error) {
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be a table")
	}
	h := http.Header{}
	for k, v := range table {
		name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(k))
		val, ok := v.(string)
		if name == "" || !ok || strings.ContainsAny(val, "\r\n") {
			return nil, fmt.Errorf("invalid header %q", k)
		}
		h.Set(name, val)
	}
	return h, nil
}

func optionsTable(raw any) (map[string]any, error) {
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be a table")
	}
	// Round-trip through JSON so the request body sees plain JSON values.
	data, err := json.Marshal(table)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateKeepAlive(v any) error {
	switch v.(type) {
	case nil, string, int, int64, float64, json.Number:
		return nil
	}
	return fmt.Errorf("must be a duration string or a number of seconds")
}

func boolPtr(v any) (*bool, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("must be a boolean")
	}
	return &b, nil
}
