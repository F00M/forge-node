// Local practice server: run the forge on a fixed port without the env lifecycle.
//   node tools/serve.ts [port] [token] [--cpu]
// --cpu prints the process CPU time used in each busy second (to see the server's share of a benchmark).

import { createApp } from '../src/app.ts';

const args = process.argv.slice(2).filter((arg) => !arg.startsWith('--'));
const port = Number.parseInt(args[0] ?? '8099', 10);
const token = args[1] ?? process.env.MAKEFUN_SHOP_TOKEN ?? 'local-token';
const server = createApp({ token });
server.listen(port, '127.0.0.1', () => {
  process.stdout.write(`[serve] forge listening on http://127.0.0.1:${port} (token ${token === '' ? 'any' : 'set'})\n`);
});

if (process.argv.includes('--cpu')) {
  let last = process.cpuUsage();
  setInterval(() => {
    const now = process.cpuUsage();
    const user = (now.user - last.user) / 1000;
    const system = (now.system - last.system) / 1000;
    last = now;
    if (user + system > 5) process.stdout.write(`[cpu] user ${user.toFixed(0)} ms, system ${system.toFixed(0)} ms, heap ${(process.memoryUsage().heapUsed / 1e6).toFixed(1)} MB\n`);
  }, 1000).unref();
}
