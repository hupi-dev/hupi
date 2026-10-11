import * as vscode from 'vscode';

/**
 * A before/after diff needs two documents for `vscode.diff` to compare,
 * but neither side is a real file on disk — this backs both with an
 * in-memory virtual document under a custom URI scheme. Shared by
 * inlineEdit.ts (one file, selection-scoped) and multiFileEdit.ts (many
 * files, whole-document) rather than each keeping its own copy of the
 * same scheme/map/provider mechanism.
 */
export interface VirtualDiffProvider {
  registration: vscode.Disposable;
  set(uri: vscode.Uri, content: string): void;
  delete(uri: vscode.Uri): void;
}

export function createVirtualDiffProvider(scheme: string): VirtualDiffProvider {
  const docs = new Map<string, string>();
  const provider: vscode.TextDocumentContentProvider = {
    provideTextDocumentContent(uri: vscode.Uri): string {
      return docs.get(uri.path) ?? '';
    },
  };
  const registration = vscode.workspace.registerTextDocumentContentProvider(scheme, provider);
  return {
    registration,
    set: (uri, content) => docs.set(uri.path, content),
    delete: (uri) => docs.delete(uri.path),
  };
}

/** Closes the diff tab showing beforeUri/afterUri, if it is still open. */
export async function closeDiffTab(beforeUri: vscode.Uri, afterUri: vscode.Uri): Promise<void> {
  for (const group of vscode.window.tabGroups.all) {
    for (const tab of group.tabs) {
      const input = tab.input;
      if (
        input instanceof vscode.TabInputTextDiff &&
        (input.original.toString() === beforeUri.toString() ||
          input.modified.toString() === afterUri.toString())
      ) {
        await vscode.window.tabGroups.close(tab);
      }
    }
  }
}
