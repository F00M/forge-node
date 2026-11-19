// The entry <-> env lifecycle: start the forge on an ephemeral port (never 8080), register it with the
// env, poll until the env says the trial is done, then stop and exit 0.
//
// The forge runs as the vendored native core (bin/forge-linux-amd64, built from core/ with Go) when the
// platform can run it; otherwise, or when the core fails to start, the in-process Node forge in src/
// serves the same API from the same code paths' TypeScript originals.

import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, existsSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createApp } from './src/app.ts';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;
const CORE_START_TIMEOUT_MS = 10_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/** POST the bound port until the env accepts it (200, or 409 when another register won). */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // Not answering yet, or the poll timed out: poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

// ---------------------------------------------------------------- the native core

interface Running {
  port: number;
  stop: () => void;
}

/** The core binary for this platform, or null when there is none to run. */
function coreBinary(): string | null {
  const override = process.env.FORGE_CORE_BIN;
  if (override !== undefined && override !== '') return existsSync(override) ? override : null;
  // The committed binary is linux/amd64; a local windows build (git-ignored) serves development runs.
  const name = process.arch !== 'x64' ? null : process.platform === 'linux' ? 'forge-linux-amd64' : process.platform === 'win32' ? 'forge-windows-amd64.exe' : null;
  if (name === null) return null;
  const path = fileURLToPath(new URL(`./bin/${name}`, import.meta.url));
  return existsSync(path) ? path : null;
}

/** Spawn the core and wait for its "LISTEN <port>" line; rejects when it exits or stays silent. */
function launch(binary: string): Promise<Running> {
  return new Promise((resolve, reject) => {
    let child: ChildProcess;
    try {
      // stdin stays an open pipe: the core exits when it closes, so it never outlives this process.
      child = spawn(binary, ['--port', '0', '--watch-stdin'], { stdio: ['pipe', 'pipe', 'inherit'], env: process.env });
    } catch (err) {
      reject(err);
      return;
    }
    let settled = false;
    let text = '';
    const fail = (why: string): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      child.kill('SIGKILL');
      reject(new Error(why));
    };
    const timer = setTimeout(() => fail('the core printed no LISTEN line in time'), CORE_START_TIMEOUT_MS);
    child.once('error', (err) => fail(`the core did not start: ${err.message}`));
    child.once('exit', (code, signal) => fail(`the core exited early (${code ?? signal})`));
    child.stdout!.setEncoding('utf8');
    child.stdout!.on('data', (chunk: string) => {
      if (settled) return;
      text += chunk;
      const match = /LISTEN (\d+)/.exec(text);
      if (match === null) return;
      settled = true;
      clearTimeout(timer);
      child.removeAllListeners('exit');
      child.on('exit', (code, signal) => {
        process.stderr.write(`[entry] the core exited (${code ?? signal})\n`);
      });
      resolve({
        port: Number.parseInt(match[1]!, 10),
        stop: () => {
          child.kill('SIGKILL');
        },
      });
    });
  });
}

/** Start the core, retrying once from a private executable copy when the vendored file cannot run. */
async function startCore(): Promise<Running | null> {
  const binary = coreBinary();
  if (binary === null) return null;
  try {
    chmodSync(binary, 0o755);
  } catch {
    // A read-only checkout: the copy below covers it.
  }
  let running: Running;
  try {
    running = await launch(binary);
  } catch (first) {
    try {
      const copy = join(mkdtempSync(join(tmpdir(), 'forge-core-')), 'forge');
      copyFileSync(binary, copy);
      chmodSync(copy, 0o755);
      running = await launch(copy);
    } catch (second) {
      process.stderr.write(`[entry] native core unavailable (${first instanceof Error ? first.message : String(first)}; ${second instanceof Error ? second.message : String(second)})\n`);
      return null;
    }
  }
  // It must answer before the env is told about it.
  try {
    const res = await fetch(`http://127.0.0.1:${running.port}/healthz`, { signal: AbortSignal.timeout(5_000) });
    if (res.ok && ((await res.json()) as { ok?: unknown }).ok === true) return running;
  } catch {
    // Fall through to the Node forge.
  }
  process.stderr.write('[entry] native core did not answer /healthz; using the Node forge\n');
  running.stop();
  return null;
}

/** The in-process Node forge (src/), the fallback. */
async function startNode(): Promise<Running> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') throw new Error('the server did not bind an ephemeral TCP port');
  return { port: address.port, stop: () => server.close() };
}

async function main(): Promise<void> {
  const core = await startCore();
  const running = core ?? (await startNode());
  process.stderr.write(`[entry] ${core !== null ? 'native core' : 'node forge'} listening on port ${running.port}\n`);
  await register(running.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');
  await waitForDone();
  running.stop();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
