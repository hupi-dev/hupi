import * as esbuild from 'esbuild';

const watch = process.argv.includes('--watch');
// Minify for real (packaged) builds; skip it under --watch so stack traces
// during local F5 development stay readable without needing sourcemaps
// open. This also meaningfully shrinks the .vsix (extension.js bundles the
// whole `openai` SDK — minification is most of what keeps that in check).
const minify = !watch;

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
  minify,
};

const webviewConfig = {
  entryPoints: ['src/webview/chat.ts'],
  bundle: true,
  outfile: 'dist/webview/chat.js',
  platform: 'browser',
  format: 'iife',
  target: 'es2022',
  sourcemap: true,
  minify,
};

const multiFileReviewWebviewConfig = {
  entryPoints: ['src/webview/multiFileReview.ts'],
  bundle: true,
  outfile: 'dist/webview/multiFileReview.js',
  platform: 'browser',
  format: 'iife',
  target: 'es2022',
  sourcemap: true,
  minify,
};

async function run() {
  if (watch) {
    const ctxExt = await esbuild.context(extensionConfig);
    const ctxWeb = await esbuild.context(webviewConfig);
    const ctxMultiFile = await esbuild.context(multiFileReviewWebviewConfig);
    await Promise.all([ctxExt.watch(), ctxWeb.watch(), ctxMultiFile.watch()]);
    console.log('watching for changes...');
  } else {
    await Promise.all([
      esbuild.build(extensionConfig),
      esbuild.build(webviewConfig),
      esbuild.build(multiFileReviewWebviewConfig),
    ]);
    console.log('build complete');
  }
}

run().catch((err) => {
  console.error(err);
  process.exit(1);
});
