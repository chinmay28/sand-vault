// Package assistant answers questions about a vault in plain language, by
// putting a chat model in front of a handful of tools that read the index.
//
// The shape is deliberately small. A Model is anything that can take a
// transcript and a list of tools and answer with text or with a request to
// call some of them; the loop in Ask runs those calls and feeds the results
// back until the model has an answer. Nothing here knows what a vault is —
// that is the Collection, in collection.go, which the server implements over
// the open index. The package can therefore be tested end to end with a
// scripted model and a fake collection, and neither a network nor a vault is
// needed to prove the loop does what it says.
//
// The model is expected to be one the user runs themselves — Ollama or vLLM
// on a machine on their own network — because the tool results carry the
// names of the films they own, and that is the one thing SAND promises never
// to hand to anybody it was not told to. See openai.go for the wire protocol
// both of those servers speak.
package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Role is who said a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn of the transcript, in the shape every chat server
// speaks: text, and for an assistant turn the tools it wants run, and for a
// tool turn which request it answers.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`

	// ToolCalls are what an assistant turn asked for. Empty means the turn is
	// an answer.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID names the call a tool turn is the result of, and Name the
	// tool that produced it.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolCall is one request from the model to run a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolSpec describes a tool to the model: what it is called, what it does,
// and the JSON schema of its arguments.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Model is a chat backend.
//
// Chat is one request: the whole transcript so far and the tools on offer, and
// in return one assistant message — either an answer or the calls the model
// wants made before it can give one.
type Model interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error)
}

// Tool is a ToolSpec with something behind it. Run gets the arguments exactly
// as the model wrote them and answers with text the model will read back.
type Tool struct {
	ToolSpec
	Run func(ctx context.Context, args json.RawMessage) (string, error)
}

// Turn is one exchange as the browser holds it: who spoke and what they said.
// Tool calls are never part of it — they are how an answer was reached, not
// part of the conversation, and replaying them would only send a stale copy
// of the index back to the model.
type Turn struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// Step is one tool the model ran on the way to an answer, kept so the reply
// can say what was looked up rather than only what was concluded.
type Step struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`

	// Error is what went wrong, when something did. The model is told the
	// same thing, and usually works around it.
	Error string `json:"error,omitempty"`
}

// Answer is what Ask produces.
type Answer struct {
	Text  string `json:"text"`
	Steps []Step `json:"steps,omitempty"`
}

// DefaultMaxSteps bounds how many rounds of tool calls one question may take.
// Every question this package is for is answered in two or three; a model that
// is still asking after eight is looping, and the cap is what stops a loop
// from running until the request times out.
const DefaultMaxSteps = 8

// ErrTooManySteps is returned when the model kept asking for tools past the
// cap without ever answering.
var ErrTooManySteps = errors.New("the assistant kept looking things up without reaching an answer")

// ErrEmptyQuestion is returned when there is nothing to ask.
var ErrEmptyQuestion = errors.New("ask something")

// Assistant is a Model, the Tools it may use, and the standing instructions
// it works under.
type Assistant struct {
	Model Model
	Tools []Tool

	// System is the instruction the transcript opens with. Empty means
	// DefaultSystemPrompt.
	System string

	// MaxSteps caps the rounds of tool calls per question. Zero means
	// DefaultMaxSteps.
	MaxSteps int
}

// Ask answers one question, given the conversation that led up to it.
//
// The history is text only; the tool calls that produced earlier answers are
// not replayed. The model sees the earlier turns so a follow-up like "and
// which of those are from the nineties?" works, and looks everything up
// afresh, which is what makes the answer about the vault as it is now.
func (a *Assistant) Ask(ctx context.Context, history []Turn, question string) (*Answer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, ErrEmptyQuestion
	}
	if a.Model == nil {
		return nil, errors.New("no model configured")
	}

	system := a.System
	if system == "" {
		system = DefaultSystemPrompt
	}
	maxSteps := a.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: RoleSystem, Content: system})
	for _, t := range history {
		if t.Role != RoleUser && t.Role != RoleAssistant {
			// Anything else in the history was never the browser's to send.
			continue
		}
		if strings.TrimSpace(t.Content) == "" {
			continue
		}
		msgs = append(msgs, Message{Role: t.Role, Content: t.Content})
	}
	msgs = append(msgs, Message{Role: RoleUser, Content: question})

	specs := make([]ToolSpec, 0, len(a.Tools))
	byName := make(map[string]Tool, len(a.Tools))
	for _, t := range a.Tools {
		specs = append(specs, t.ToolSpec)
		byName[t.Name] = t
	}

	answer := &Answer{}
	for step := 0; step <= maxSteps; step++ {
		reply, err := a.Model.Chat(ctx, msgs, specs)
		if err != nil {
			return nil, err
		}
		reply.Role = RoleAssistant
		msgs = append(msgs, reply)

		if len(reply.ToolCalls) == 0 {
			answer.Text = strings.TrimSpace(reply.Content)
			if answer.Text == "" {
				return nil, errors.New("the assistant answered with nothing")
			}
			return answer, nil
		}
		if step == maxSteps {
			break
		}

		// Every result goes back in its own tool turn, in the order the calls
		// were made, before the model is asked again — which is the contract
		// the chat servers hold to and the one thing that lets a model ask
		// for two lookups at once.
		for _, call := range reply.ToolCalls {
			result, runErr := a.run(ctx, byName, call)
			s := Step{Tool: call.Name, Arguments: call.Arguments}
			if runErr != nil {
				s.Error = runErr.Error()
				result = "error: " + runErr.Error()
			}
			answer.Steps = append(answer.Steps, s)
			msgs = append(msgs, Message{
				Role:       RoleTool,
				Content:    result,
				ToolCallID: call.ID,
				Name:       call.Name,
			})
		}
	}
	return nil, ErrTooManySteps
}

// run executes one call. A tool the model made up, or one that fails, is
// reported back to the model as text rather than ending the question: a model
// told "no such tool" picks a real one, and one told a lookup failed says so.
func (a *Assistant) run(ctx context.Context, tools map[string]Tool, call ToolCall) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	tool, ok := tools[call.Name]
	if !ok || tool.Run == nil {
		return "", fmt.Errorf("no such tool %q", call.Name)
	}
	args := call.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return tool.Run(ctx, args)
}

// DefaultSystemPrompt is what the assistant is told about itself.
//
// The rule that matters is the second paragraph: answers come from the tools,
// not from what the model remembers. A local model's memory of a film series
// is patchy, and a confident list of Batman films that is missing two of them
// is worse than no list — so the database is asked, every time, and the model
// is told to say what it compared.
const DefaultSystemPrompt = `You are the assistant built into SAND Vault, a private file store. The person you are talking to owns the vault. The tools you have read their index: the files and folders in it, and the film details stored against the videos they have matched against a film database.

Answer only from what the tools return. Never rely on your own memory of what films exist, which films are in a series, or what the vault contains. If you are asked what is missing from a collection, list what the vault holds with list_films, search the film database with search_film_database for the series or subject asked about, and compare the two by title and year. Say plainly which titles are in the vault and which are not, and mention that the comparison was made against the film database's search results.

Keep answers short and concrete. Use plain sentences and, where a list is the answer, a simple list with one title and year per line. If a tool reports an error or the vault has no film details, say so rather than guessing. Do not invent files, titles or years.`
