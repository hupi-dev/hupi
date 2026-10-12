# HUPI agent: Windows end-to-end test

This suite tests the HUPI agent ([docs/AGENT.md](../../../docs/AGENT.md)) on a
real Windows 11 desktop, in both apps that ship it:

- **stock VS Code** with the HUPI extension installed from the Marketplace;
- **HUPI Code**, from a hupi-dev/hupi-code CI build (`hupi-code-win32-x64`),
  with its bundled extension.

It drives HUPI's sidebar the way a user does. It types a prompt, clicks
Allow or Deny on HUPI's approval cards, clicks Yes on VS Code's own
dialogs, and presses Stop. It takes screenshots along the way. Then it
checks the result in the chat, on disk and in HUPI's memory.

The unit tests (`npm test`) mock VS Code, and the HUPI Code smoke test checks
that the plumbing works. This suite exercises what they can't on Windows:
- real paths (drive letters, `\`);
- PowerShell and VS Code's terminal shell integration;
- the webview UI;
- MCP servers started by VS Code itself;
- the gateway's memory rules over a real agent turn.

## What it checks

Both apps run two scenarios. Each run starts from a fresh profile and a
fresh test project, `C:\Users\Public\hupi e2e's <app>`. The folder name has
a space and an apostrophe, to test command quoting.

**main**: "read, edit, run, use MCP tools"
| Step | Expected |
|---|---|
| `read_file src/config.ts` | Runs without asking (read-only) |
| `edit_file` (41 → 42) | HUPI's card asks while VS Code's diff view shows the change. After Allow, the file on disk says 42 |
| `run_command` | The card asks. The command runs in the visible "HUPI agent" PowerShell terminal, in the project folder, and its output (`E2E_OK export const answer = 42;`) reaches the model |
| MCP `smoke_echo` (read-only) | Runs with no prompt |
| MCP `smoke_write` (not read-only) | VS Code's own "Run smoke_write" dialog appears. After Yes, the tool runs |
| Memory | Exactly one episode for the turn, tagged `agent,tool:read_file,tool:edit_file,tool:run_command,tool:mcp_smoke_smoke_echo,tool:mcp_smoke_smoke_write` |

**scenario2**: "refusals, Deny and Stop"
| Step | Expected |
|---|---|
| `read_file .env` | Refused: "doesn't read files that commonly hold secrets" |
| `read_file C:\Users\Public\…\src\config.ts` (absolute) | Works |
| `edit_file` (41 → 99) | Deny is clicked. The file stays at 41 |
| `run_command` (`Start-Sleep 120`) | Allow is clicked, then Stop while it runs. The terminal's command is interrupted and the card says "stopped" |
| Memory | No episode for the stopped turn |

In HUPI Code the suite leaves `hupi.agent.enabled` unset, so it also checks
that the agent is on by default there. In stock VS Code it turns the
setting on, since it's off by default.

## How it works (methodology)

```
 host (Linux)                                   Windows 11 VM (dockur/windows)
 ─────────────────────────────────────          ─────────────────────────────────────────
 host/run-suite.sh                               Z:\ = host's shared folder
   ├─ start.sh: scripted model (127.0.0.1:18802)    Z:\e2e\run.ps1 -App … -Mode …
   │            HUPI gateway (172.17.0.1:18790) ◀─── extension (hupi.baseUrl)
   │            test DB hupi_win_e2e                 ├─ fresh profile, mcp.json, project
   ├─ vmtype.py: types run.ps1 into Win+R  ──────▶   ├─ app launched with DevTools on :9333
   └─ check.py: result.json + DB episodes ◀──────    └─ driver.js (CDP): prompt, clicks, screenshots
```

- **The model is scripted** (`host/scripted_model.py`). The model's choices
  are fixed, so the run is deterministic and needs no API key. It streams
  tool calls in fragments, the way OpenAI and Anthropic do. Everything else
  is real: the extension, the gateway (built from this checkout), Postgres,
  VS Code or HUPI Code, PowerShell and the MCP server. The suite does
  **not** test how well a real model picks tools.
- **The gateway runs on the host**, listening on Docker's default bridge
  (`172.17.0.1`), which the VM reaches through dockur's NAT. It uses its own
  database, `hupi_win_e2e`, so checking memory just means counting episodes
  before and after each run.
- **The MCP server** (`guest/smoke-mcp.js`) is a tiny stdio server run by
  the app's own binary with `ELECTRON_RUN_AS_NODE=1`, so the VM needs no
  Node. It's configured in the profile's user `mcp.json`, and VS Code
  starts it when HUPI asks.
- **Driving the UI.**
  - `run.ps1` launches the app with `--remote-debugging-port=9333`.
  - `driver.js` runs on the same binary as Node and talks to the app over
    the Chrome DevTools Protocol.
  - It finds HUPI's chat webview, sets the input and clicks Send, then
    clicks each approval card's Allow (or Deny, or Stop).
  - VS Code's own dialogs are set to `window.dialogStyle: custom`, so they're
    in the DOM and clickable. Stock VS Code's first-run onboarding is
    clicked through.
  - Screenshots come from `Page.captureScreenshot`.
  - A tiny helper extension (`host/opener/`) opens HUPI's sidebar at startup.
- **Getting into the VM.** `vmtype.py` presses Win+R and types the
  `run.ps1` command through QEMU's monitor (`sendkey`). Then it waits
  for `Z:\e2e\out-<app>-<mode>\DONE`. `vmshot.sh` saves the VM's whole
  screen, which helps when something hangs.

## Running it

### Requirements (host)

- Linux with KVM (`/dev/kvm`) and Docker. About 70 GB of disk for the VM.
- Postgres 16 with pgvector, reachable by a superuser.
  - The suite creates `hupi_win_e2e` and runs `schema/migrate.sh` on it.
  - The gateway connects as `hupi_app`, using the test password that
    [`master-test.sh`](../../../master-test.sh) uses. The same Postgres
    you run that gate against works.
- Ollama with `nomic-embed-text`, for the gateway's embeddings
  (`host/providers.yaml`).
- Go, Python 3 with Pillow (only for `vmshot.sh`), `psql`, and `gh`
  (to download HUPI Code CI builds).
- `npm ci` in `vscode-extension/`, which provides `vsce` for packaging the
  helper extension.

### 1. Create the VM (once)

```bash
vscode-extension/e2e/windows/vm/start-vm.sh
```

The first run downloads Windows 11 IoT Enterprise LTSC (an evaluation copy,
valid 90 days) and installs it unattended. Then `vm/oem/setup.ps1` installs
VS Code. Watch it at http://127.0.0.1:8006. It's ready when
`~/hupi-win/shared/setup-status.txt` says `setup done`. Later runs of
`start-vm.sh` just start the existing VM. `HUPI_WIN_DIR` (default
`~/hupi-win`) and `HUPI_WIN_CONTAINER` (default `hupi-win`) change where
it lives and what it's called.

### 2. Run the suite

```bash
cd vscode-extension/e2e/windows/host
HUPI_E2E_ADMIN_DB='postgres://postgres:hupi@localhost:15432/hupi_win_e2e?sslmode=disable' \
HUPI_E2E_APP_DB='postgres://hupi_app:hupi_app_test_only@localhost:15432/hupi_win_e2e?sslmode=disable' \
./run-suite.sh --hupi-code-run <hupi-code CI run id>
```

- **HUPI Code build:** `--hupi-code-run` downloads that run's
  `hupi-code-win32-x64` artifact into the shared folder. Find the run id
  with `gh run list --repo hupi-dev/hupi-code`. Without it, the build
  already in the shared folder is used.
- **Choosing apps:** `./run-suite.sh vscode` or `./run-suite.sh hupicode`
  runs just one app.
- **Database URLs:** the defaults point at `localhost:5432`.
- **Duration:** a full run (2 apps × 2 scenarios) takes about 10 minutes.

The VM must be logged in at its desktop, which happens after it boots. The
suite prints a PASS/FAIL line for each check, and `ALL PASSED` at the end.
Each run's screenshots, `result.json` and logs land in
`~/hupi-win/shared/e2e/out-<app>-<mode>/`. The gateway's and the model's
logs go to `~/.cache/hupi-win-e2e/run/`.

### Which extension gets tested

- **Stock VS Code:** whatever the Marketplace serves at the time, the
  published release.
- **HUPI Code:** the extension that CI build bundled. That's
  `vscode-extension/` on hupi-dev/hupi `main` at the moment the build ran.
- **The gateway:** built from this checkout.

To test unreleased extension changes in stock VS Code, put a `.vsix` in the
shared folder and swap the `--install-extension hupi.hupi-vscode` line in
`guest/run.ps1` for its `Z:\` path.

### Troubleshooting

- **A run times out:** look at `~/.cache/hupi-win-e2e/timeout-<app>-<mode>.png`
  and at the run's `drive.log` and `transcript.txt`.
- **"chat webview not found":** HUPI's sidebar didn't open. Check that the
  helper extension installed (`extensions.txt` in the run's folder).
- **In-VM downloads are slow.** Put large files in the shared folder from the
  host instead.

## Results

| Date | Stock VS Code | HUPI Code | Notes |
|---|---|---|---|
| 2026-10-11 | Marketplace 0.1.16: main ✅ scenario2 ✅ | hupi-code#10 CI build (bundled 0.1.16): main ✅ scenario2 ✅ | First run. Before the fix in hupi-dev/hupi#157, the tools refused every file and every absolute path on Windows |
