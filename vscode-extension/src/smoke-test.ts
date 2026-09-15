// Manual, non-vscode smoke test: exercises the real hupiClient module
// against a live, already-running HUPI gateway — proving the actual
// request/response cycle (including streaming) works, not just that the
// code compiles. Not part of `npm test` (that's the pure unit suite);
// run this by hand with a gateway already up:
//
//   HUPI_SMOKE_BASE_URL=http://localhost:8787 \
//   HUPI_SMOKE_API_KEY=... \
//   npm run smoke
//
// Both env vars are optional — defaults match a Tier 1/2 local gateway
// with no auth required (HUPI_REQUIRE_AUTH unset).
import { createClient, streamChat } from './hupiClient';

async function main(): Promise<void> {
  const baseUrl = process.env.HUPI_SMOKE_BASE_URL ?? 'http://localhost:8787';
  const apiKey = process.env.HUPI_SMOKE_API_KEY ?? '';
  const model = process.env.HUPI_SMOKE_MODEL ?? '';

  console.log(`smoke test: POST ${baseUrl}/v1/chat/completions (streamed)`);
  const client = createClient({ baseUrl, apiKey, model });

  let full = '';
  const full2 = await streamChat(client, {
    model,
    messages: [{ role: 'user', content: 'Reply with exactly the word: pong' }],
    onDelta: (delta) => {
      full += delta;
      process.stdout.write(delta);
    },
  });
  process.stdout.write('\n');

  if (full2 !== full) {
    throw new Error('streamChat return value did not match accumulated deltas');
  }
  if (full.trim() === '') {
    throw new Error('empty response from HUPI — check the gateway is running and reachable');
  }
  console.log('smoke test passed: received a non-empty streamed response.');
}

main().catch((err) => {
  console.error('smoke test FAILED:', err);
  process.exit(1);
});
