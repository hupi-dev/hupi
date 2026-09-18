import path from 'node:path';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  resolve: {
    alias: {
      // No real `vscode` package exists outside a real VS Code process —
      // this makes every `import * as vscode from 'vscode'` resolve to
      // the hand-written mock instead, so files that need it
      // (extension.ts, chatViewProvider.ts, inlineEdit.ts, secrets.ts,
      // config.ts, oidcAuth.ts) are testable too, not just the two files
      // (hupiClient.ts, oidc.ts) that were kept vscode-free specifically
      // to sidestep this. TypeScript still type-checks `'vscode'` imports
      // against the real @types/vscode ambient declarations regardless —
      // this alias only affects what actually runs at test time.
      vscode: path.resolve(__dirname, 'src/test/vscode-mock.ts'),
    },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
});
