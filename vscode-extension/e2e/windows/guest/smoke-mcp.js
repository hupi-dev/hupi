// Test MCP server: smoke_echo is read-only; smoke_write is not, so VS Code asks first.
const rl = require('readline').createInterface({ input: process.stdin });
const reply = (id, result) => process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id, result }) + '\n');
rl.on('line', (line) => {
  if (!line.trim()) return;
  const m = JSON.parse(line);
  if (m.id === undefined) return;
  if (m.method === 'initialize') reply(m.id, { protocolVersion: (m.params && m.params.protocolVersion) || '2025-06-18', capabilities: { tools: {} }, serverInfo: { name: 'smoke', version: '0' } });
  else if (m.method === 'tools/list') reply(m.id, { tools: [
    { name: 'smoke_echo', description: 'Echo text back.', inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] }, annotations: { readOnlyHint: true } },
    { name: 'smoke_write', description: 'Pretend to write something.', inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] } },
  ] });
  else if (m.method === 'tools/call') reply(m.id, { content: [{ type: 'text', text: (m.params.name === 'smoke_write' ? 'wrote:' : 'smoke:') + ((m.params.arguments || {}).text || '') }] });
  else reply(m.id, {});
});
