import { execFileSync, spawn } from 'node:child_process';
import { resolve } from 'node:path';
import process from 'node:process';
import {
  assertSecretAbsent,
  cleanupSmokeEnvironment,
  desktopDirectory,
  prepareSmokeEnvironment,
  smokeBootstrapPassword
} from './smoke-support.mjs';

const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync('rustc', ['--print', 'host-tuple'], { encoding: 'utf8' }).trim();
const extension = targetTriple.includes('windows') ? '.exe' : '';
const targetDirectory = process.env.CARGO_TARGET_DIR
  ? resolve(process.env.CARGO_TARGET_DIR)
  : resolve(desktopDirectory, 'src-tauri', 'target');
const executable = resolve(targetDirectory, 'debug', `cyberstrike-desktop${extension}`);
const smoke = prepareSmokeEnvironment();

try {
  const result = await runDesktop(executable, {
    ...smoke.environment,
    CYBERSTRIKE_DESKTOP_SMOKE_TIMEOUT_MS: '30000'
  });
  if (result.code !== 0 || result.signal !== null) {
    throw new Error(`desktop smoke failed: ${JSON.stringify(result)}`);
  }
  if (!result.output.includes('desktop core bootstrap required')) {
    throw new Error('desktop core did not request first-launch bootstrap');
  }
  if (!result.output.includes('desktop core ready')) {
    throw new Error('desktop core did not reach READY');
  }
  assertSecretAbsent(smoke.environment.CYBERSTRIKE_DESKTOP_TEST_ROOT, smokeBootstrapPassword);
  console.log(`desktop core lifecycle smoke passed: elapsed_ms=${result.elapsed}`);
} finally {
  cleanupSmokeEnvironment(smoke);
}

function runDesktop(command, environment) {
  return new Promise((resolvePromise, reject) => {
    const startedAt = Date.now();
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
      reject(new Error('desktop lifecycle smoke did not exit within 45 seconds'));
    }, 45_000);
    child.once('error', (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once('close', (code, signal) => {
      clearTimeout(timeout);
      resolvePromise({ code, signal, elapsed: Date.now() - startedAt, output });
    });
  });
}
