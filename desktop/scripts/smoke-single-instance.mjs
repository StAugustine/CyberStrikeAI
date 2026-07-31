import { execFileSync, spawn } from 'node:child_process';
import { resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const scriptDirectory = fileURLToPath(new URL('.', import.meta.url));
const desktopDirectory = resolve(scriptDirectory, '..');
const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync('rustc', ['--print', 'host-tuple'], { encoding: 'utf8' }).trim();
const extension = targetTriple.includes('windows') ? '.exe' : '';
const targetDirectory = process.env.CARGO_TARGET_DIR
  ? resolve(process.env.CARGO_TARGET_DIR)
  : resolve(desktopDirectory, 'src-tauri', 'target');
const executable = resolve(targetDirectory, 'debug', `cyberstrike-desktop-poc${extension}`);

const first = spawn(executable, [], {
  env: {
    ...process.env,
    CYBERSTRIKE_DESKTOP_POC_SMOKE_TIMEOUT_MS: '20000',
    CYBERSTRIKE_DESKTOP_POC_HOLD_AFTER_VERIFY_MS: '3000'
  },
  stdio: ['ignore', 'pipe', 'pipe']
});
let firstOutput = '';
for (const stream of [first.stdout, first.stderr]) {
  stream.on('data', (chunk) => {
    const text = chunk.toString();
    firstOutput += text;
    process.stderr.write(text);
  });
}

const firstReady = waitForOutput(
  () => firstOutput.includes('desktop PoC browser protocols verified'),
  15_000,
  'first instance did not verify browser protocols'
);
const firstExit = waitForExit(first, 'first instance');
await firstReady;

const second = spawn(executable, [], {
  env: process.env,
  stdio: 'inherit'
});
const secondResult = await waitForExit(second, 'second instance');
const firstResult = await firstExit;

if (secondResult.code !== 0 || secondResult.signal !== null) {
  throw new Error(`second instance failed: ${JSON.stringify(secondResult)}`);
}
if (firstResult.code !== 0 || firstResult.signal !== null) {
  throw new Error(`first instance failed: ${JSON.stringify(firstResult)}`);
}
if (!firstOutput.includes('desktop PoC existing instance focused')) {
  throw new Error('first instance did not receive the second-instance callback');
}
console.log('desktop PoC single-instance smoke passed');

function waitForOutput(predicate, timeoutMilliseconds, message) {
  return new Promise((resolvePromise, reject) => {
    const startedAt = Date.now();
    const interval = setInterval(() => {
      if (predicate()) {
        clearInterval(interval);
        resolvePromise();
      } else if (Date.now() - startedAt >= timeoutMilliseconds) {
        clearInterval(interval);
        first.kill();
        reject(new Error(message));
      }
    }, 25);
  });
}

function waitForExit(child, label) {
  return new Promise((resolvePromise, reject) => {
    const timeout = setTimeout(() => {
      child.kill();
      reject(new Error(`${label} did not exit within 30 seconds`));
    }, 30_000);
    child.once('error', (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once('close', (code, signal) => {
      clearTimeout(timeout);
      resolvePromise({ code, signal });
    });
  });
}
