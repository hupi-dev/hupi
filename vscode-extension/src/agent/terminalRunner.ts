import * as vscode from 'vscode';

// Runs a command for HUPI's agent in a visible "HUPI agent" terminal,
// through VS Code's shell integration (Terminal.shellIntegration
// .executeCommand), so the user sees it run and the agent gets the real
// output and exit code. Verified in a HUPI Code build: shell integration
// comes up in well under a second for bash, and output carries terminal
// escape sequences that are stripped here.

const TERMINAL_NAME = 'HUPI agent';
const SHELL_INTEGRATION_WAIT_MS = 10_000;

export interface CommandResult {
  exitCode: number | undefined;
  output: string;
  timedOut: boolean;
}

let terminal: vscode.Terminal | undefined;

/** Strips OSC/CSI escape sequences and carriage returns from terminal output. */
export function cleanTerminalOutput(raw: string): string {
  return raw
    .replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, '')
    .replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, '')
    .replace(/\x1b[@-_]/g, '')
    .replace(/\r/g, '');
}

function waitForShellIntegration(t: vscode.Terminal, ms: number): Promise<vscode.TerminalShellIntegration | undefined> {
  if (t.shellIntegration) {
    return Promise.resolve(t.shellIntegration);
  }
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      sub.dispose();
      resolve(undefined);
    }, ms);
    const sub = vscode.window.onDidChangeTerminalShellIntegration((e) => {
      if (e.terminal === t) {
        clearTimeout(timer);
        sub.dispose();
        resolve(e.shellIntegration);
      }
    });
  });
}

/**
 * Runs command in cwd. Resolves when it exits, times out, or signal
 * aborts; on timeout or abort the command is interrupted (Ctrl+C).
 * Output beyond maxChars is cut off.
 */
export async function runInTerminal(command: string, cwd: vscode.Uri, timeoutMs: number, maxChars: number, signal: AbortSignal): Promise<CommandResult> {
  if (!terminal || terminal.exitStatus !== undefined) {
    terminal = vscode.window.createTerminal({ name: TERMINAL_NAME, cwd });
  }
  const t = terminal;
  t.show(true);
  const si = await waitForShellIntegration(t, SHELL_INTEGRATION_WAIT_MS);
  if (!si) {
    throw new Error("the terminal's shell integration isn't available (it needs bash, zsh, fish or PowerShell), so HUPI can't read command output");
  }
  // cd first so the command runs where the agent asked, even in a reused terminal.
  const execution = si.executeCommand(inDirectory(cwd.fsPath, command, process.platform));

  let output = '';
  let truncated = false;
  const reading = (async () => {
    for await (const chunk of execution.read()) {
      if (output.length < maxChars) {
        output += chunk;
      } else {
        truncated = true;
      }
    }
  })();

  let timer: ReturnType<typeof setTimeout> | undefined;
  let endSub: vscode.Disposable | undefined;
  const onAbort = () => t.sendText('\x03', false);
  signal.addEventListener('abort', onAbort);
  try {
    const outcome = await new Promise<{ exitCode: number | undefined } | 'timeout' | 'aborted'>((resolve) => {
      endSub = vscode.window.onDidEndTerminalShellExecution((e) => {
        if (e.execution === execution) {
          resolve({ exitCode: e.exitCode });
        }
      });
      timer = setTimeout(() => resolve('timeout'), timeoutMs);
      signal.addEventListener('abort', () => resolve('aborted'));
    });
    if (outcome === 'aborted') {
      throw new Error('cancelled');
    }
    if (outcome === 'timeout') {
      t.sendText('\x03', false);
    }
    // Let the output stream finish (it ends with the execution).
    await Promise.race([reading, new Promise((r) => setTimeout(r, 1000))]);
    let cleaned = cleanTerminalOutput(output).slice(0, maxChars);
    if (truncated || output.length >= maxChars) {
      cleaned += '\n… (output truncated)';
    }
    return { exitCode: outcome === 'timeout' ? undefined : outcome.exitCode, output: cleaned, timedOut: outcome === 'timeout' };
  } finally {
    clearTimeout(timer);
    endSub?.dispose();
    signal.removeEventListener('abort', onAbort);
  }
}

/** The command, run after changing to dir: PowerShell on Windows (the
 *  shell VS Code's shell integration supports there), POSIX shells
 *  (bash, zsh, fish 3+) elsewhere. */
export function inDirectory(dir: string, command: string, platform: string): string {
  if (platform === 'win32') {
    return `Set-Location -LiteralPath '${dir.replace(/'/g, "''")}'; ${command}`;
  }
  return `cd '${dir.replace(/'/g, "'\\''")}' && ${command}`;
}

/** For tests: forget the cached terminal. */
export function __resetTerminal(): void {
  terminal = undefined;
}
