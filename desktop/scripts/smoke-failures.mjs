import { execFileSync, spawn } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { resolve } from 'node:path';
import process from 'node:process';
import {
  cleanupSmokeEnvironment,
  desktopDirectory,
  prepareSmokeEnvironment
} from './smoke-support.mjs';

const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync('rustc', ['--print', 'host-tuple'], { encoding: 'utf8' }).trim();
const extension = targetTriple.includes('windows') ? '.exe' : '';
const targetDirectory = process.env.CARGO_TARGET_DIR
  ? resolve(process.env.CARGO_TARGET_DIR)
  : resolve(desktopDirectory, 'src-tauri', 'target');
const executable = resolve(targetDirectory, 'debug', `cyberstrike-desktop${extension}`);
const smoke = prepareSmokeEnvironment();
const brokenResources = resolve(smoke.directory, 'broken-defaults');
mkdirSync(brokenResources, { recursive: true });

try {
  const result = await runDesktop(executable, {
    ...smoke.environment,
    CYBERSTRIKE_DESKTOP_RESOURCE_DIR: brokenResources
  });
  if (result.code === 0 || result.signal !== null) {
    throw new Error(`unexpected invalid-resource result: ${JSON.stringify(result)}`);
  }
  if (!result.output.includes('desktop core terminated unexpectedly')) {
    throw new Error('desktop shell did not report the failed core startup');
  }
  console.log(`desktop core invalid-resource smoke passed: code=${result.code}`);
} finally {
  cleanupSmokeEnvironment(smoke);
}

function runDesktop(command, environment) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, [], {
      env: { ...process.env, ...environment },
      stdio: ['ignore', 'pipe', 'pipe']
    });
    let output = '';
    for (const stream of [child.stdout, child.stderr]) {
      stream.on('data', (chunk) => {
        const text = chunk.toString();
        output += text;
        process.stderr.write(text);
      });
    }
    const timeout = setTimeout(() => {
      child.kill();
      reject(new Error('desktop failure smoke did not exit within 30 seconds'));
    }, 30_000);
    child.once('error', (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once('close', (code, signal) => {
      clearTimeout(timeout);
      resolvePromise({ code, signal, output });
    });
  });
}
