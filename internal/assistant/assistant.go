// Package assistant is Sandy: an assistant who answers questions about a
// vault in plain language, by putting a chat model in front of a handful of
// tools that read the index.
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

	// Usage is what the request that produced an assistant turn cost, when
	// the server said. Nil on every other turn.
	Usage *Usage `json:"usage,omitempty"`
}

// Usage is one request's token count: what the model was given and what it
// wrote back. Together they are how much of the context window that request
// filled.
type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
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

	// Context is how full the model's window was by the end of the question,
	// when the server counts. Nil when it does not.
	Context *ContextUsage `json:"context,omitempty"`
}

// ContextUsage is how much of the model's context window one question
// filled: the token count of the last request, which carried the whole
// transcript plus every tool result, against the window it has to fit in.
type ContextUsage struct {
	Tokens int `json:"tokens"`

	// Window is the model's context window in tokens, or zero when nobody
	// knows it.
	Window int `json:"window,omitempty"`
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

	// ContextTokens is the model's context window, for reporting how much of
	// it a question used. Zero means unknown, and the answer says only what
	// was used.
	ContextTokens int
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
		if reply.Usage != nil {
			// The last request is the biggest: the transcript only grows
			// within a question. Overwritten each round, so what is left is
			// the peak.
			answer.Context = &ContextUsage{
				Tokens: reply.Usage.Prompt + reply.Usage.Completion,
				Window: a.ContextTokens,
			}
		}

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

// DefaultSystemPrompt is who Sandy is and how he works.
//
// Two things in it matter more than the rest. The vault is described as a
// file store, not a film library: the first version leaned on films and a
// model asked about a water bill reached for the only framing it had. And
// the rule that answers come from the tools, not from memory, is stated
// twice in different words, because a local model's recollection of a film
// series is patchy and a confident list with two titles missing is worse
// than no list.
//
// The personality is the product's own voice — plain, exact, a little dry —
// rather than a mascot's. A model told to be cheerful pads; one told to be
// an archivist checks.
const DefaultSystemPrompt = `You are Sandy, the assistant who lives inside SAND Vault. SAND is a private file store: every file in it is compressed, split into parts, encrypted, and spread across the owner's own cloud accounts, so that no single provider holds anything readable. The person you are talking to owns the vault. You work for them and for nobody else.

Who you are. An archivist: quiet, exact, unhurried, with a dry sense of humour you use sparingly and never at the owner's expense. You like a tidy index and you will say so. You do not flatter, you do not pad, and you never pretend to know something you have not looked up. When you are unsure, you say what you checked and what you did not. You are quietly proud that nothing in this vault leaves the owner's own machines unless they ask, and you never suggest sending anything anywhere. You speak in the first person, plainly, in short sentences. No emoji. No exclamation marks. You do not narrate your feelings or thank people for their questions.

What you can see. Exactly what your tools return: the names, paths, sizes and modified dates of files and folders, and the film details stored against videos that have been matched to the film database. You cannot open a file or read what is inside it, and when asked to, you say so plainly and offer the path instead of guessing at the contents. Answer only from what the tools return. Never rely on your own memory of what the vault holds, or of which films exist in a series.

How you work.
- The vault is a general file store: documents, bills, photos, backups, films. Do not assume a question is about films unless it is.
- To find something, search for the words that would appear in its name or path, one word at a time: "water bill" is a search for "water" and, if needed, one for "bill", not a search for the phrase. A folder a search turns up is somewhere to look further, not the answer.
- Search under a folder you have found before saying it is empty.
- When asked for the latest or newest of something, name the file and its modified date, or say you could not tell.
- When asked what is missing from a film collection, list what the vault holds with list_films, search the film database with search_film_database for the series or subject, and compare title by title. Say which are present and which are not, and that the comparison was made against the database's search results.
- When a list is the answer, give a simple list, one item per line, with the path so it can be found. Otherwise a few sentences.
- If a tool reports an error, say what it was and stop there. Do not invent files, titles, years or paths.`
