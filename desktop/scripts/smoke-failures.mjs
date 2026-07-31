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

const crash = await runDesktop({
  CYBERSTRIKE_DESKTOP_POC_SIDECAR_CRASH_MS: '750'
});
if (crash.code === 0 || crash.signal !== null) {
  throw new Error(`unexpected sidecar crash result: ${JSON.stringify(crash)}`);
}
console.log(`desktop PoC unexpected-crash smoke passed: code=${crash.code}`);

const forced = await runDesktop({
  CYBERSTRIKE_DESKTOP_POC_IGNORE_SHUTDOWN: '1',
  CYBERSTRIKE_DESKTOP_POC_SMOKE_TIMEOUT_MS: '20000'
});
if (forced.code === 0 || forced.signal !== null || forced.elapsed < 5000) {
  throw new Error(`unexpected forced-shutdown result: ${JSON.stringify(forced)}`);
}
console.log(`desktop PoC forced-shutdown smoke passed: code=${forced.code} elapsed_ms=${forced.elapsed}`);

function runDesktop(environment) {
  return new Promise((resolvePromise, reject) => {
    const startedAt = Date.now();
    const child = spawn(executable, [], {
      env: { ...process.env, ...environment },
      stdio: 'inherit'
    });
    const timeout = setTimeout(() => {
      child.kill();
      reject(new Error('desktop PoC failure smoke did not exit within 30 seconds'));
    }, 30_000);
    child.once('error', (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once('close', (code, signal) => {
      clearTimeout(timeout);
      resolvePromise({ code, signal, elapsed: Date.now() - startedAt });
    });
  });
}
