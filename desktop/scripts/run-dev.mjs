import { execFileSync, spawn } from 'node:child_process';
import { resolve } from 'node:path';
import process from 'node:process';
import {
  cleanupSmokeEnvironment,
  desktopDirectory,
  prepareSmokeEnvironment
} from './smoke-support.mjs';

execFileSync(process.execPath, [resolve(desktopDirectory, 'scripts', 'build-sidecar.mjs')], {
  cwd: desktopDirectory,
  env: process.env,
  stdio: 'inherit'
});

const development = prepareSmokeEnvironment();
const tauriCli = resolve(desktopDirectory, 'node_modules', '@tauri-apps', 'cli', 'tauri.js');
const child = spawn(process.execPath, [tauriCli, 'dev'], {
  cwd: desktopDirectory,
  env: { ...process.env, ...development.environment },
  stdio: 'inherit'
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.once(signal, () => child.kill(signal));
}

child.once('error', (error) => {
  cleanupSmokeEnvironment(development);
  throw error;
});
child.once('close', (code, signal) => {
  cleanupSmokeEnvironment(development);
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exitCode = code ?? 1;
  }
});
