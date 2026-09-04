package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ChatCompletions is a Model over the chat completions protocol, which is the
// one wire shape every local model server speaks: Ollama, vLLM, llama.cpp,
// LM Studio and the rest all serve POST {base}/chat/completions and GET
// {base}/models with the same request and response bodies. Targeting the
// protocol rather than any one server is what makes the choice of server a
// URL in the settings rather than a decision in the code.
//
// Only what the loop needs is implemented: one non-streaming request, with
// tools, and a reachability check. A stray field the server sends back is
// ignored; a field it needs and does not get is a bug here, not a
// configuration problem, so nothing about the body is optional.
type ChatCompletions struct {
	// BaseURL is the server's OpenAI-compatible root, "http://host:11434/v1"
	// for Ollama and "http://host:8000/v1" for vLLM.
	BaseURL string

	// Model names which of the models the server holds to talk to.
	Model string

	// APIKey is sent as a bearer token when set. A home server has no
	// authentication of its own, so this is normally empty; vLLM can be
	// started with one.
	APIKey string

	// HTTPClient is the client to use, or http.DefaultClient when nil.
	HTTPClient *http.Client
}

// RequestTimeout bounds one chat request. Minutes rather than seconds, because
// the first request after a model server has been idle loads the model into
// memory before it answers, and a 14B model off a disk takes a while.
const RequestTimeout = 5 * time.Minute

// ErrNotConfigured is returned before any request when there is no server to
// send it to.
var ErrNotConfigured = errors.New("no assistant model has been configured")

// ErrNoSuchModel is returned by Ping when the server answers but does not
// hold the model that was asked for.
var ErrNoSuchModel = errors.New("the model server does not have that model")

// A pinned interface check: this is the Model the server wires up.
var _ Model = (*ChatCompletions)(nil)

// ValidateBaseURL reports whether a base URL is one a request could go to.
// It is also what the settings handler checks before it stores one.
func ValidateBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", ErrNotConfigured
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("that is not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("the model server URL must start with http:// or https://")
	}
	if u.Host == "" {
		return "", errors.New("the model server URL needs a host")
	}
	return raw, nil
}

func (c *ChatCompletions) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// The wire shapes. Arguments travel as a JSON string inside JSON, which is
// the protocol's one oddity and the reason ToolCall.Arguments is a RawMessage
// on this side of it.
type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireCallFunction `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

type wireResponse struct {
	Choices []struct {
		Message wireMessage `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chat sends the transcript and returns the model's next message.
func (c *ChatCompletions) Chat(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
	base, err := ValidateBaseURL(c.BaseURL)
	if err != nil {
		return Message{}, err
	}
	if strings.TrimSpace(c.Model) == "" {
		return Message{}, ErrNotConfigured
	}

	req := wireRequest{Model: c.Model, Messages: make([]wireMessage, 0, len(msgs))}
	for _, m := range msgs {
		wm := wireMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, call := range m.ToolCalls {
			args := string(call.Arguments)
			if args == "" {
				args = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID: call.ID, Type: "function",
				Function: wireCallFunction{Name: call.Name, Arguments: args},
			})
		}
		req.Messages = append(req.Messages, wm)
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, wireTool{Type: "function", Function: wireFunction(t)})
	}

	var resp wireResponse
	if err := c.post(ctx, base+"/chat/completions", req, &resp); err != nil {
		return Message{}, err
	}
	if resp.Error != nil {
		return Message{}, fmt.Errorf("the model server refused the request: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return Message{}, errors.New("the model server answered with no message")
	}

	wm := resp.Choices[0].Message
	out := Message{Role: RoleAssistant, Content: wm.Content}
	if resp.Usage != nil {
		out.Usage = &Usage{Prompt: resp.Usage.PromptTokens, Completion: resp.Usage.CompletionTokens}
	}
	for i, call := range wm.ToolCalls {
		id := call.ID
		if id == "" {
			// A server that does not number its calls still needs the results
			// matched back to them.
			id = fmt.Sprintf("call_%d", i)
		}
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return Message{}, fmt.Errorf("the model wrote arguments for %s that are not JSON", call.Function.Name)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID: id, Name: call.Function.Name, Arguments: json.RawMessage(args),
		})
	}
	return out, nil
}

// ModelInfo is what the server says about the configured model.
type ModelInfo struct {
	// ContextTokens is the context window, or zero when the server did not
	// say. vLLM reports it on its model list; Ollama on its own show call.
	ContextTokens int

	// Tools reports whether the model can call tools, when the server says
	// either way. Ollama lists a model's capabilities; most servers do not.
	Tools *bool
}

// ErrNoToolSupport is returned by Describe when the server says outright
// that the model cannot call tools, which is the one thing Sandy needs it
// to do.
var ErrNoToolSupport = errors.New("the model server says that model cannot call tools")

// Ping checks that the server answers and holds the configured model. It
// is Describe with the answer thrown away.
func (c *ChatCompletions) Ping(ctx context.Context) error {
	_, err := c.Describe(ctx)
	return err
}

// Describe checks that the server answers and holds the configured model,
// and reports what it will say about it: the context window, and whether
// the model can call tools.
//
// It is what storing the settings runs, so that a mistyped address, a model
// that was never pulled, or a model that cannot call tools fails in the
// settings dialog rather than on the first question.
//
// The context window matters because Ollama runs every model at a window of
// its own choosing — 4096 tokens unless told otherwise — regardless of what
// the model was trained for, and a transcript that outgrows it is silently
// cut from the front. Knowing the number is what lets the panel show how
// close a conversation is to that.
func (c *ChatCompletions) Describe(ctx context.Context) (ModelInfo, error) {
	base, err := ValidateBaseURL(c.BaseURL)
	if err != nil {
		return ModelInfo{}, err
	}
	model := strings.TrimSpace(c.Model)
	if model == "" {
		return ModelInfo{}, errors.New("name the model to use")
	}

	var listed struct {
		Data []struct {
			ID string `json:"id"`
			// vLLM's model list carries the window; Ollama's does not.
			MaxModelLen int `json:"max_model_len"`
		} `json:"data"`
	}
	if err := c.get(ctx, base+"/models", &listed); err != nil {
		return ModelInfo{}, err
	}

	info := ModelInfo{}
	found := false
	names := make([]string, 0, len(listed.Data))
	for _, m := range listed.Data {
		if m.ID == model {
			found = true
			info.ContextTokens = m.MaxModelLen
			break
		}
		names = append(names, m.ID)
	}
	if !found {
		if len(names) == 0 {
			return ModelInfo{}, fmt.Errorf("%w: it lists no models at all", ErrNoSuchModel)
		}
		return ModelInfo{}, fmt.Errorf("%w: it has %s", ErrNoSuchModel, strings.Join(names, ", "))
	}

	// Ollama's own show call, beside the compatible endpoint, is the only
	// place it says how big the window is and whether the model can call
	// tools. Any other server answers 404 here, which is not an error: it is
	// simply not Ollama.
	if err := c.describeOllama(ctx, base, model, &info); err != nil {
		return ModelInfo{}, err
	}
	return info, nil
}

// describeOllama asks Ollama's native API about the model, when the server
// turns out to be Ollama. It fills what it learns into info and is silent
// about a server that does not answer the call at all.
func (c *ChatCompletions) describeOllama(ctx context.Context, base, model string, info *ModelInfo) error {
	origin := strings.TrimSuffix(base, "/v1")
	if origin == base {
		return nil
	}

	var shown struct {
		Parameters   string                     `json:"parameters"`
		ModelInfo    map[string]json.RawMessage `json:"model_info"`
		Capabilities []string                   `json:"capabilities"`
	}
	err := c.post(ctx, origin+"/api/show", map[string]string{"model": model}, &shown)
	if err != nil {
		var status statusError
		if errors.As(err, &status) {
			// Not Ollama, or an Ollama too old to have the call.
			return nil
		}
		return err
	}

	// What the model was trained to hold.
	for key, raw := range shown.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		var n int
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			info.ContextTokens = n
		}
	}
	// What it is actually run at, when the Modelfile says.
	for _, line := range strings.Split(shown.Parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "num_ctx" {
			var n int
			if _, err := fmt.Sscanf(fields[1], "%d", &n); err == nil && n > 0 {
				info.ContextTokens = n
			}
		}
	}

	if shown.Capabilities != nil {
		tools := false
		for _, cap := range shown.Capabilities {
			if cap == "tools" {
				tools = true
			}
		}
		info.Tools = &tools
		if !tools {
			return fmt.Errorf("%w: pull one that can, such as qwen3:14b", ErrNoToolSupport)
		}
	}
	return nil
}

func (c *ChatCompletions) get(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *ChatCompletions) post(ctx context.Context, target string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

// maxResponseBytes bounds what is read back. An answer is a few kilobytes; a
// megabyte is a server that has gone wrong.
const maxResponseBytes = 4 << 20

func (c *ChatCompletions) do(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the model server: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reading the model server's answer: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The protocol puts a message under "error"; a proxy in the way puts
		// whatever it likes. Show the first line of either.
		var e wireResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != nil && e.Error.Message != "" {
			return statusError{resp.StatusCode, e.Error.Message}
		}
		line := strings.TrimSpace(string(raw))
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		if line == "" {
			line = http.StatusText(resp.StatusCode)
		}
		return statusError{resp.StatusCode, line}
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the model server's answer was not JSON: %w", err)
	}
	return nil
}

// statusError is an answer the server gave that was not success: reachable,
// but refusing. Telling it apart from a server that never answered is what
// lets an optional call be skipped on a 404 and reported on a timeout.
type statusError struct {
	code    int
	message string
}

func (e statusError) Error() string {
	return fmt.Sprintf("the model server answered %d: %s", e.code, e.message)
}
