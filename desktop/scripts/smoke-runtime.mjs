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

const startedAt = Date.now();
const child = spawn(executable, [], {
  env: {
    ...process.env,
    CYBERSTRIKE_DESKTOP_POC_SMOKE_TIMEOUT_MS: '20000'
  },
  stdio: 'inherit'
});
const timeout = setTimeout(() => {
  child.kill();
}, 30_000);

child.on('error', (error) => {
  clearTimeout(timeout);
  console.error(`failed to start desktop PoC: ${error.message}`);
  process.exitCode = 1;
});
child.on('close', (code, signal) => {
  clearTimeout(timeout);
  const elapsed = Date.now() - startedAt;
  if (code !== 0 || signal !== null) {
    console.error(`desktop PoC smoke failed: code=${code} signal=${signal} elapsed_ms=${elapsed}`);
    process.exitCode = 1;
    return;
  }
  console.log(`desktop PoC smoke passed: elapsed_ms=${elapsed}`);
});
