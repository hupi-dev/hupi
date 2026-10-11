# The HUPI agent

HUPI's sidebar chat can work as an agent: to answer, it can read and search
your code, check problems, edit or create files, run commands, and use tools
from your MCP servers. It asks before anything changes. HUPI memory works
as usual and remembers the conversation, not what the tools returned.

It's a preview:
- **HUPI Code:** on by default. It's that editor's only agent, since HUPI
  Code doesn't ship Copilot.
- **VS Code:** turn on `hupi.agent.enabled` in the HUPI extension (0.1.16+).

## What it can do

| Tool | What it does | Asks first? |
|---|---|---|
| `read_file` | Read a file, or a range of lines | No (read-only) |
| `list_files` | List files matching a glob, e.g. `src/**/*.ts` | No |
| `search_text` | Search file contents, plain text or regex | No |
| `get_diagnostics` | The problems VS Code reports, for one file or all | No |
| `get_selection` | The file and text you have selected | No |
| `edit_file` | Replace one exact piece of text in a file | **Yes, every time**, with a diff |
| `create_file` | Create a new file (never overwrites) | **Yes, every time**, with a preview |
| `run_command` | Run a shell command and read its output | **Yes, every time**, showing the exact command |
| MCP tools | Tools from the MCP servers in your `mcp.json` | VS Code asks before any that isn't read-only |

Each tool call shows up as a card in the reply, with its arguments, its
status and a collapsible result. **Stop** cancels a reply at any point,
including a running command.

### Approvals

- **Edits and new files:** the tool's card asks Allow/Deny, and VS Code's
  diff view opens with the exact change. Nothing is written unless you
  allow it. An applied edit is in the editor's undo history, and the file
  is saved.
- **Commands:** the card shows the exact command line and folder. If you
  allow it, the command runs in a visible **HUPI agent** terminal. It has
  a timeout (60 seconds by default), and Stop interrupts it.
- **MCP tools:** VS Code shows its own confirmation for any tool its
  server doesn't mark as read-only.
- **`hupi.agent.approval: "all"`:** HUPI also asks on the card before the
  read-only tools.

## What HUPI remembers

An agent turn is one question answered over several steps. HUPI stores it
once, when the final answer arrives:
- **Stored:** your question and the final answer, tagged `agent` and
  `tool:<name>` for each tool used. They're consolidated and searchable
  like any chat turn.
- **Never stored:** tool arguments and tool output (file contents, command
  output, MCP results). Those can be large, secret, or hostile text that
  shouldn't become "facts".
- **A turn you stop or that fails:** stores nothing.

HUPI also searches your memory once per turn, not once per step, so every
step sees the same context. Plain chat (agent off, or a reply that uses no
tools) is unchanged. See [API_REFERENCE.md](API_REFERENCE.md#post-v1chatcompletions)
for the exact rules.

## Safety limits

- **Workspace only.** Paths resolve inside your open workspace folders:
  - `..` can't climb out, and an absolute path must be inside a folder;
  - on a local disk, a symlink pointing outside the workspace is refused;
  - `node_modules`, `.git` and build output are skipped.
- **No secret files.** The agent won't read, search, edit or create files
  that commonly hold secrets, because their contents would go to your
  model provider:
  - `.env*` (but not `.env.example`/`.sample`/`.template`);
  - private keys and keystores (`*.pem`, `*.key`, `id_rsa`, …);
  - anything under `.ssh/`, plus `.aws/credentials`, `.netrc`, `.npmrc`,
    `.pypirc` and `.git-credentials`.
- **Untrusted workspaces.** In an untrusted workspace (VS Code's
  Restricted Mode), the edit, create and run tools aren't offered at all.
- **HUPI's tools stay with HUPI.** They aren't registered with VS Code, so
  other agents in VS Code gain nothing through HUPI.
- **Limits per turn.**
  - Each tool result sent to the model is capped
    (`hupi.agent.maxToolResultChars`).
  - After `hupi.agent.maxSteps` rounds of tools, the model must answer.
  - A whole turn stops after 10 minutes.

## MCP servers

The agent uses the servers configured in VS Code's `mcp.json`, your user
one or the workspace's (**MCP: Open User Configuration**). At the start of
each turn HUPI starts them and offers their tools.
- **HUPI memory's own MCP server is never offered** here, because the chat
  already uses your memory.
- **HUPI Code:** MCP servers work there even without Copilot. Its build
  smoke test starts a test server and calls its tool every time.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `hupi.agent.enabled` | off (on in HUPI Code) | Let the sidebar chat use tools |
| `hupi.agent.workspaceTools` | on | The read-only workspace tools |
| `hupi.agent.writeTools` | on | `edit_file`, `create_file`, `run_command` (never in an untrusted workspace) |
| `hupi.agent.vscodeTools` | on | Tools from your MCP servers |
| `hupi.agent.approval` | `changes` | `all` also asks before read-only tools |
| `hupi.agent.maxSteps` | 10 | Rounds of tool calls per turn before a final answer |
| `hupi.agent.maxToolResultChars` | 20000 | Longest tool result sent to the model |

## Requirements and troubleshooting

- **A model that can call tools.** OpenAI, Claude, and local models such as
  `qwen2.5` all can. Very small local models may answer without calling
  tools, or call them clumsily. A `providers.yaml` profile with
  `supports_tools: false` makes the gateway refuse tool requests with a
  clear error.
- **`run_command` needs terminal shell integration:** bash, zsh, fish or
  PowerShell. Otherwise HUPI says so instead of guessing at the output.
- **The public demo** (`hupi-demo`) doesn't allow tool calling.
- **Team workspaces** (`hupi.teamId`) work the same. The turn is stored in
  the team's memory, with you as the actor.

## How it's built

- **The extension runs the loop.** The gateway only relays tool calls in
  OpenAI's format and applies the memory rules above.
- **Code:**
  - `vscode-extension/src/agent/`: the loop, the tool registry, the
    workspace, write and terminal tools;
  - `internal/gateway/agentturn.go`: the gateway's memory rules;
  - `internal/provider/tools.go`: tool calling for OpenAI-compatible
    providers and Anthropic.
- **Details:** [VSCODE_EXTENSION.md](VSCODE_EXTENSION.md) and
  [ARCHITECTURE.md](../ARCHITECTURE.md).
