package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A stand-in for the model server, answering the way Ollama's and vLLM's
// OpenAI-compatible endpoints do, and keeping what it was sent.
type fakeServer struct {
	*httptest.Server
	models   []string
	reply    map[string]any
	status   int
	requests []map[string]any
	auth     string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{models: []string{"qwen3:14b", "llama3.1:8b"}, status: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.auth = r.Header.Get("Authorization")
		data := []map[string]any{}
		for _, m := range f.models {
			data = append(data, map[string]any{"id": m, "object": "model"})
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.auth = r.Header.Get("Authorization")
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.requests = append(f.requests, req)
		w.WriteHeader(f.status)
		json.NewEncoder(w).Encode(f.reply)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func assistantReply(content string, calls ...map[string]any) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "qwen3:14b",
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": "stop"}},
	}
}

func TestChatSendsTheProtocolShapeAndReadsToolCallsBack(t *testing.T) {
	f := newFakeServer(t)
	f.reply = assistantReply("", map[string]any{
		"id": "call_abc", "type": "function",
		"function": map[string]any{"name": "list_films", "arguments": `{"dir": "/films"}`},
	})

	c := &ChatCompletions{BaseURL: f.URL + "/v1/", Model: "qwen3:14b"}
	msgs := []Message{
		{Role: RoleSystem, Content: "be brief"},
		{Role: RoleUser, Content: "what films do I have?"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "old", Name: "search_vault", Arguments: json.RawMessage(`{"query":"x"}`)}}},
		{Role: RoleTool, ToolCallID: "old", Name: "search_vault", Content: `{"count":0}`},
	}
	tools := []ToolSpec{{Name: "list_films", Description: "lists", Parameters: schema(`{"type":"object"}`)}}

	got, err := c.Chat(context.Background(), msgs, tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("got %+v, want one tool call", got)
	}
	tc := got.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Name != "list_films" || string(tc.Arguments) != `{"dir": "/films"}` {
		t.Errorf("tool call %+v", tc)
	}

	// What went over the wire.
	req := f.requests[0]
	if req["model"] != "qwen3:14b" || req["stream"] != false {
		t.Errorf("request model/stream: %v / %v", req["model"], req["stream"])
	}
	sent := req["messages"].([]any)
	if len(sent) != 4 {
		t.Fatalf("sent %d messages, want 4", len(sent))
	}
	assistantTurn := sent[2].(map[string]any)
	calls := assistantTurn["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search_vault" || fn["arguments"] != `{"query":"x"}` {
		t.Errorf("replayed call %+v — arguments must travel as a string", fn)
	}
	toolTurn := sent[3].(map[string]any)
	if toolTurn["role"] != "tool" || toolTurn["tool_call_id"] != "old" || toolTurn["content"] != `{"count":0}` {
		t.Errorf("tool turn %+v", toolTurn)
	}
	wireTools := req["tools"].([]any)
	spec := wireTools[0].(map[string]any)
	if spec["type"] != "function" || spec["function"].(map[string]any)["name"] != "list_films" {
		t.Errorf("tool spec %+v", spec)
	}
	if f.auth != "" {
		t.Errorf("an Authorization header %q was sent with no key set", f.auth)
	}
}

func TestChatReadsAPlainAnswer(t *testing.T) {
	f := newFakeServer(t)
	f.reply = assistantReply("You have three.")
	c := &ChatCompletions{BaseURL: f.URL + "/v1", Model: "qwen3:14b", APIKey: "secret"}

	got, err := c.Chat(context.Background(), []Message{{Role: RoleUser, Content: "how many?"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "You have three." || len(got.ToolCalls) != 0 {
		t.Errorf("got %+v", got)
	}
	if f.auth != "Bearer secret" {
		t.Errorf("Authorization %q", f.auth)
	}
	if _, has := f.requests[0]["tools"]; has {
		t.Error("an empty tools list was sent; the protocol wants it absent")
	}
}

func TestChatNumbersCallsAServerLeftUnnumbered(t *testing.T) {
	f := newFakeServer(t)
	f.reply = assistantReply("",
		map[string]any{"function": map[string]any{"name": "a", "arguments": ""}},
		map[string]any{"function": map[string]any{"name": "b", "arguments": `{"q":1}`}},
	)
	c := &ChatCompletions{BaseURL: f.URL + "/v1", Model: "qwen3:14b"}
	got, err := c.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolCalls[0].ID != "call_0" || got.ToolCalls[1].ID != "call_1" {
		t.Errorf("ids %q %q", got.ToolCalls[0].ID, got.ToolCalls[1].ID)
	}
	if string(got.ToolCalls[0].Arguments) != "{}" {
		t.Errorf("empty arguments became %q", got.ToolCalls[0].Arguments)
	}
}

func TestChatRejectsArgumentsThatAreNotJSON(t *testing.T) {
	f := newFakeServer(t)
	f.reply = assistantReply("", map[string]any{"function": map[string]any{"name": "a", "arguments": "{not json"}})
	c := &ChatCompletions{BaseURL: f.URL + "/v1", Model: "qwen3:14b"}
	if _, err := c.Chat(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err %v", err)
	}
}

func TestChatSurfacesTheServersError(t *testing.T) {
	f := newFakeServer(t)
	f.status = http.StatusNotFound
	f.reply = map[string]any{"error": map[string]any{"message": `model "nope" not found, try pulling it first`}}
	c := &ChatCompletions{BaseURL: f.URL + "/v1", Model: "nope"}
	_, err := c.Chat(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "try pulling it first") {
		t.Fatalf("err %v", err)
	}
}

func TestChatNeedsAServerAndAModel(t *testing.T) {
	if _, err := (&ChatCompletions{}).Chat(context.Background(), nil, nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no URL: %v", err)
	}
	if _, err := (&ChatCompletions{BaseURL: "http://x"}).Chat(context.Background(), nil, nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no model: %v", err)
	}
	if _, err := (&ChatCompletions{BaseURL: "ftp://x", Model: "m"}).Chat(context.Background(), nil, nil); err == nil {
		t.Error("an ftp URL was accepted")
	}
}

func TestPingChecksTheModelIsThere(t *testing.T) {
	f := newFakeServer(t)

	if err := (&ChatCompletions{BaseURL: f.URL + "/v1", Model: "qwen3:14b"}).Ping(context.Background()); err != nil {
		t.Errorf("a model the server lists: %v", err)
	}

	err := (&ChatCompletions{BaseURL: f.URL + "/v1", Model: "mistral"}).Ping(context.Background())
	if !errors.Is(err, ErrNoSuchModel) || !strings.Contains(err.Error(), "qwen3:14b, llama3.1:8b") {
		t.Errorf("a model it does not: %v", err)
	}

	f.models = nil
	if err := (&ChatCompletions{BaseURL: f.URL + "/v1", Model: "qwen3:14b"}).Ping(context.Background()); !errors.Is(err, ErrNoSuchModel) {
		t.Errorf("an empty server: %v", err)
	}

	if err := (&ChatCompletions{BaseURL: "http://127.0.0.1:1", Model: "m"}).Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("a server that is not there: %v", err)
	}
}

func TestValidateBaseURL(t *testing.T) {
	cases := map[string]bool{
		"http://gaming-pc:11434/v1":   true,
		"https://llm.home.arpa/v1/":   true,
		"http://192.168.1.20:8000/v1": true,
		"":                            false,
		"gaming-pc:11434":             false,
		"ftp://gaming-pc/v1":          false,
		"http://":                     false,
	}
	for raw, ok := range cases {
		got, err := ValidateBaseURL(raw)
		if (err == nil) != ok {
			t.Errorf("%q: err %v, want ok=%v", raw, err, ok)
		}
		if ok && strings.HasSuffix(got, "/") {
			t.Errorf("%q kept its trailing slash: %q", raw, got)
		}
	}
}
