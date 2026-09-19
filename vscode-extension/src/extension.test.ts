import { beforeEach, describe, expect, it } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock } from './test/vscode-mock';
import { activate, deactivate } from './extension';

function fakeContext(): vscode.ExtensionContext {
  const subscriptions: { dispose(): void }[] = [];
  return {
    subscriptions,
    secrets: {
      get: async () => undefined,
      store: async () => {},
      delete: async () => {},
    },
    extensionUri: vscode.Uri.file('/fake/extension'),
  } as unknown as vscode.ExtensionContext;
}

beforeEach(() => {
  __resetVscodeMock();
});

describe('activate', () => {
  it('registers every command and view provider, all as disposables', () => {
    const context = fakeContext();

    activate(context);

    // hupi.setApiKey (1) + hupi.signIn/hupi.signOut (2) + inlineEdit's
    // [contentProvider, command] (2) + multiFileEdit's
    // [contentProvider, command] (2) + the inline completion provider (1)
    // + the chat webview view provider (1) + the hupi.chat participant (1).
    // A future registration that forgets to push its disposable would
    // shrink this count — that's the point of asserting the exact number,
    // not just "at least one."
    expect(context.subscriptions.length).toBe(10);
    for (const sub of context.subscriptions) {
      expect(typeof (sub as { dispose?: unknown }).dispose).toBe('function');
    }
  });

  it('does not throw when called', () => {
    expect(() => activate(fakeContext())).not.toThrow();
  });
});

describe('deactivate', () => {
  it('does not throw', () => {
    expect(() => deactivate()).not.toThrow();
  });
});
