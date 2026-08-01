import { execFileSync, spawn } from 'node:child_process';
import { resolve } from 'node:path';
import process from 'node:process';
import {
  cleanupSmokeEnvironment,
  desktopDirectory,
  prepareSmokeEnvironment
} from './smoke-support.mjs';
import { writeTauriBuildConfig } from './build-config.mjs';

const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync('rustc', ['--print', 'host-tuple'], { encoding: 'utf8' }).trim();
const environment = { ...process.env, CYBERSTRIKE_DESKTOP_TARGET: targetTriple };

execFileSync(process.execPath, [resolve(desktopDirectory, 'scripts', 'build-sidecar.mjs')], {
  cwd: desktopDirectory,
  env: environment,
  stdio: 'inherit'
});

const development = prepareSmokeEnvironment();
const tauriCli = resolve(desktopDirectory, 'node_modules', '@tauri-apps', 'cli', 'tauri.js');
const tauriConfig = writeTauriBuildConfig(targetTriple);
const child = spawn(process.execPath, [tauriCli, 'dev', '--config', tauriConfig], {
  cwd: desktopDirectory,
  env: { ...environment, ...development.environment },
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
