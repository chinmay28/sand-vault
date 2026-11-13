# Sandy, the archivist

Sandy is a chat assistant in the web UI that answers questions about your vault
in plain words, such as *which films in a series am I missing?* or *where are my
water bills?* This page covers what Sandy can see, how to give him a model, and
how to read the context meter. Sandy is browser-only. There is no CLI command.

## What Sandy is

Sandy is a language model **you run yourself**, on a machine of your own (Ollama
or vLLM on the PC with the graphics card, say), reached over your network. Sand
Vault puts the model in front of three tools that read the index:

| Tool | Returns |
|---|---|
| `list_films` | the videos matched to a film, with their titles |
| `search_vault` | files and folders whose names match a query |
| `search_film_database` | matches from the film database for a title, series or character |

Sandy sees only what those tools return: names, paths, sizes, modified dates and
the film titles stored against your videos. He never sees a file's contents, an
account, or a key. The only lookup that leaves your network is the film database
search, which sends the title searched for and nothing about the vault, just like
matching a folder (see [films.md](films.md)).

Each answer lists what Sandy looked up, as tags under the reply, so you can judge
it by its sources. One question can take up to eight rounds of tool calls. The
transcript lives in your browser and is sent with each question. The server keeps
nothing between questions, so locking the vault ends the conversation.

## Giving Sandy a model

1. Run a model server that speaks the OpenAI-compatible chat completions
   protocol. Ollama, vLLM, llama.cpp and LM Studio all do.
2. Make it listen on your network, not only on loopback. For Ollama, set
   `OLLAMA_HOST=0.0.0.0`.
3. Pull a model that can call tools. On a 16 GB card, `qwen3:14b` or
   `gpt-oss:20b` fit entirely in memory. On less, use `llama3.1:8b`.
4. Open **✦ Sandy** in the header, then **Set Sandy up**, or go to **Vault
   settings › Sandy**, and fill in:

| Field | Example | Notes |
|---|---|---|
| Model server | `http://gaming-pc:11434/v1` | Ollama is `:11434/v1`, vLLM `:8000/v1`; must be `http://` or `https://` |
| Model | `qwen3:14b` | one the server already holds |
| Context window | `32768` | optional; leave empty to trust what the server reports |
| Token | | optional; sent as a bearer token (for example, a vLLM started with an API key). Stored, never shown again |

Saving checks that the server answers and lists the model. A PC that is off, or
a model you haven't pulled, fails in the dialog rather than on your first
question. On Ollama it also checks the model's capabilities and refuses one that
can't call tools.

The settings are sealed in the vault file. They are not in the `manifest.sand`
copies on your accounts, and not in a recovery kit, so a recovered vault needs
Sandy set up again.

## The context meter

Each answer carries the server's token count for the last request, which is the
whole transcript plus every tool result. The panel draws it against the model's
context window, for example *6.2k of 40k · 15%*. It turns amber at 75% and red
at 90%. Past that, the model starts losing the beginning of the conversation.
**Clear** starts a new conversation.

vLLM reports the window in its model list, and Ollama in its model details.
**Ollama runs every model at 4,096 tokens unless `OLLAMA_CONTEXT_LENGTH` says
otherwise**, whatever the model was trained for, and silently drops the start of
a longer conversation. If Sandy forgets how a chat began, this is why. Raise
`OLLAMA_CONTEXT_LENGTH` and enter the real figure under *Context window* so the
meter measures against it.

## Security

The model server usually has no authentication, and it receives your file and
film names. Keep it on a network you trust, the same advice as for the Sand Vault
port itself (see [security.md](../security.md)).
