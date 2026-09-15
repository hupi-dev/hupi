import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'node',
    // Only src/hupiClient.ts and its test are exercised here — anything
    // importing `vscode` (extension.ts, chatViewProvider.ts, inlineEdit.ts,
    // secrets.ts, config.ts) can't run outside a real VS Code host and is
    // deliberately excluded, not accidentally skipped.
    include: ['src/**/*.test.ts'],
  },
});
