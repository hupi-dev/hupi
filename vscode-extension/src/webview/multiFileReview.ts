// Runs inside the review panel's webview iframe — must never import
// `vscode` (see webview/chat.ts's identical note). Bundled separately by
// esbuild.mjs's multiFileReviewConfig.

declare function acquireVsCodeApi(): {
  postMessage: (message: unknown) => void;
};

type ToWebview = { type: 'render'; files: { path: string; checked: boolean }[] };

const vscode = acquireVsCodeApi();
const filesEl = document.getElementById('files') as HTMLDivElement;
const applyButton = document.getElementById('apply') as HTMLButtonElement;
const discardButton = document.getElementById('discard') as HTMLButtonElement;

function render(files: { path: string; checked: boolean }[]): void {
  filesEl.innerHTML = '';
  for (const file of files) {
    const row = document.createElement('div');
    row.className = 'file-row';

    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.checked = file.checked;
    checkbox.addEventListener('change', () => {
      vscode.postMessage({ type: 'toggle', path: file.path, checked: checkbox.checked });
    });

    const path = document.createElement('span');
    path.className = 'path';
    path.textContent = file.path;

    const viewDiff = document.createElement('button');
    viewDiff.textContent = 'View Diff';
    viewDiff.addEventListener('click', () => {
      vscode.postMessage({ type: 'viewDiff', path: file.path });
    });

    row.append(checkbox, path, viewDiff);
    filesEl.appendChild(row);
  }
}

applyButton.addEventListener('click', () => {
  const paths = Array.from(filesEl.querySelectorAll<HTMLInputElement>('input[type=checkbox]:checked')).map(
    (cb) => cb.closest('.file-row')!.querySelector('.path')!.textContent!,
  );
  vscode.postMessage({ type: 'apply', paths });
});

discardButton.addEventListener('click', () => {
  vscode.postMessage({ type: 'discard' });
});

window.addEventListener('message', (event: MessageEvent<ToWebview>) => {
  if (event.data.type === 'render') {
    render(event.data.files);
  }
});
