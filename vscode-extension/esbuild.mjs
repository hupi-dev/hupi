import * as esbuild from 'esbuild';

const watch = process.argv.includes('--watch');

// Two separate bundles: the extension host runs in Node and must not bundle
// `vscode` (it's provided by the host at runtime); the webview runs in a
// plain browser context inside an iframe and must not import `vscode` at
// all (see src/webview/chat.ts's own comment).
const extensionConfig = {
  entryPoints: ['src/extension.ts'],
  bundle: true,
  outfile: 'dist/extension.js',
  platform: 'node',
  format: 'cjs',
  target: 'node18',
  external: ['vscode'],
  sourcemap: true,
};

const webviewConfig = {
  entryPoints: ['src/webview/chat.ts'],
  bundle: true,
  outfile: 'dist/webview/chat.js',
  platform: 'browser',
  format: 'iife',
  target: 'es2022',
  sourcemap: true,
};

async function run() {
  if (watch) {
    const ctxExt = await esbuild.context(extensionConfig);
    const ctxWeb = await esbuild.context(webviewConfig);
    await Promise.all([ctxExt.watch(), ctxWeb.watch()]);
    console.log('watching for changes...');
  } else {
    await Promise.all([esbuild.build(extensionConfig), esbuild.build(webviewConfig)]);
    console.log('build complete');
  }
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
