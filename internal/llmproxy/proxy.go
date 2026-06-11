// Package llmproxy runs a tiny in-process reverse proxy in front of an
// OpenAI-compatible LLM endpoint so that testdiag works with models whose
// native tool-calling syntax differs from what AgenticGoKit understands.
//
// Why this exists: AgenticGoKit v0.5.x's OpenAI adapter does NOT do native tool
// calling — it never sends a `tools` array and reads only
// choices[].message.content from the response. The agent then parses tool calls
// out of that text. Models like GPT-OSS, Gemma, Mistral and Nemotron emit their
// own tool-call syntaxes, which that parser doesn't recognize. This proxy sits
// between the adapter and the real server and fixes both ends:
//
//   - Request side:  injects a `tools` array (so tool-aware chat templates
//     advertise the tools to the model) when tool schemas are provided.
//   - Response side:  rewrites whatever tool-call format the model emitted —
//     native syntax in the content, or a structured tool_calls field — into the
//     canonical TOOL_CALL{...} text the agent reliably parses (see toolproto).
//
// Point cfg.LLM.BaseURL at BaseURL() and the rest of the program is unchanged.
package llmproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gilbertr/testdiag/internal/toolproto"
)

// Tool is a tool definition advertised to the model via the request's `tools`
// array. Parameters is a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

// Proxy is a running normalizing reverse proxy. Close it when done.
type Proxy struct {
	listener net.Listener
	server   *http.Server
	baseURL  string
}

// Start launches the proxy in front of upstreamBaseURL (e.g.
// http://localhost:1234/v1), listening on an ephemeral localhost port. If tools
// is non-empty, it is injected into every chat-completions request. The proxy
// is serving by the time Start returns.
func Start(upstreamBaseURL string, tools []Tool) (*Proxy, error) {
	target, err := url.Parse(strings.TrimSuffix(upstreamBaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parsing upstream base URL %q: %w", upstreamBaseURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("upstream base URL %q must be absolute (scheme://host)", upstreamBaseURL)
	}

	openAITools := toOpenAITools(tools)

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			reqPath := req.URL.Path
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = singleJoin(target.Path, reqPath)
			req.URL.RawPath = ""
			req.Host = target.Host
			// Ask for an unencoded body so ModifyResponse can rewrite it.
			req.Header.Set("Accept-Encoding", "identity")
			if isChatCompletions(reqPath) && len(openAITools) > 0 {
				injectTools(req, openAITools)
			}
		},
		ModifyResponse: normalizeResponse,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting proxy listener: %w", err)
	}
	p := &Proxy{
		listener: ln,
		server:   &http.Server{Handler: rp},
		baseURL:  fmt.Sprintf("http://%s", ln.Addr().String()),
	}
	go p.server.Serve(ln)
	return p, nil
}

// BaseURL is the local URL to use as the LLM base URL. It carries no path
// suffix; the adapter appends "/chat/completions" and the proxy re-prefixes the
// upstream path.
func (p *Proxy) BaseURL() string { return p.baseURL }

// Close shuts the proxy down.
func (p *Proxy) Close() error { return p.server.Close() }

// injectTools adds (or augments) the request's `tools` array and defaults
// tool_choice to "auto". Best-effort: on any decode problem the body is left
// untouched so a normal request still goes through.
func injectTools(req *http.Request, tools []map[string]interface{}) {
	if req.Body == nil {
		return
	}
	raw, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		req.Body = io.NopCloser(bytes.NewReader(raw))
		return
	}
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		req.Body = io.NopCloser(bytes.NewReader(raw))
		req.ContentLength = int64(len(raw))
		return
	}
	if _, exists := body["tools"]; !exists {
		body["tools"] = tools
	}
	if _, exists := body["tool_choice"]; !exists {
		body["tool_choice"] = "auto"
	}
	setBody(req, body, raw)
}

// normalizeResponse rewrites a chat-completions JSON response so that any
// tool call (native syntax in content, or a structured tool_calls field)
// becomes canonical TOOL_CALL text in message.content. Non-JSON and streaming
// responses pass through untouched.
func normalizeResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return nil // streaming (text/event-stream) or anything non-JSON
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not the shape we expected; pass it through verbatim.
		restoreBody(resp, raw)
		return nil
	}

	changed := false
	if choices, ok := body["choices"].([]interface{}); ok {
		for _, ch := range choices {
			choice, ok := ch.(map[string]interface{})
			if !ok {
				continue
			}
			msg, ok := choice["message"].(map[string]interface{})
			if !ok {
				continue
			}
			if rewriteMessage(msg) {
				changed = true
			}
		}
	}

	if !changed {
		restoreBody(resp, raw)
		return nil
	}
	out, err := json.Marshal(body)
	if err != nil {
		restoreBody(resp, raw)
		return nil
	}
	restoreBody(resp, out)
	return nil
}

// rewriteMessage folds a structured tool_calls field and any native tool-call
// syntax in the content into TOOL_CALL text. Returns whether it changed msg.
func rewriteMessage(msg map[string]interface{}) bool {
	content, _ := msg["content"].(string)
	var pieces []string

	if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
		if text := toolproto.FromStructured(tc); text != "" {
			pieces = append(pieces, text)
			delete(msg, "tool_calls")
		}
	}

	normalized := toolproto.Normalize(content)
	if normalized != content || len(pieces) > 0 {
		pieces = append(pieces, normalized)
		msg["content"] = strings.TrimLeft(strings.Join(pieces, "\n"), "\n")
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func toOpenAITools(tools []Tool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		params := t.Parameters
		if params == nil {
			params = map[string]interface{}{"type": "object"}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

func isChatCompletions(path string) bool {
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/completions")
}

// singleJoin joins two URL path segments with exactly one slash.
func singleJoin(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
	}
}

func setBody(req *http.Request, body map[string]interface{}, fallback []byte) {
	out, err := json.Marshal(body)
	if err != nil {
		out = fallback
	}
	req.Body = io.NopCloser(bytes.NewReader(out))
	req.ContentLength = int64(len(out))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(out)))
}

func restoreBody(resp *http.Response, data []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
	// We requested identity upstream; make sure no stale encoding header remains.
	resp.Header.Del("Content-Encoding")
}
