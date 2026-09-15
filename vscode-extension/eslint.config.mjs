// @ts-check
import tsPlugin from '@typescript-eslint/eslint-plugin';
import tsParser from '@typescript-eslint/parser';

export default [
  {
    files: ['src/**/*.ts'],
    languageOptions: {
      parser: tsParser,
      parserOptions: { project: './tsconfig.json' },
    },
    plugins: { '@typescript-eslint': tsPlugin },
    rules: {
      ...tsPlugin.configs.recommended.rules,
      // Message payloads crossing the extension<->webview postMessage
      // boundary and third-party response shapes are legitimately handled
      // as `any` in a couple of narrow spots (see the inline comments at
      // each use) — off globally rather than littering // eslint-disable
      // comments through code that's otherwise fully typed.
      '@typescript-eslint/no-explicit-any': 'off',
    },
  },
  {
    ignores: ['dist/**', 'out/**', 'node_modules/**'],
  },
];
