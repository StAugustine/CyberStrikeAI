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

let first;
try {
  first = spawn(executable, [], {
    env: {
      ...process.env,
      ...smoke.environment,
      CYBERSTRIKE_DESKTOP_SMOKE_TIMEOUT_MS: '30000',
      CYBERSTRIKE_DESKTOP_HOLD_AFTER_READY_MS: '3000'
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

  const firstExit = waitForExit(first, 'first instance');
  await waitForOutput(
    () => firstOutput.includes('desktop core ready'),
    25_000,
    'first instance did not reach READY'
  );

  const second = spawn(executable, [], {
    env: { ...process.env, ...smoke.environment },
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
  if (!firstOutput.includes('desktop existing instance focused')) {
    throw new Error('first instance did not receive the second-instance callback');
  }
  assertSecretAbsent(smoke.environment.CYBERSTRIKE_DESKTOP_TEST_ROOT, smokeBootstrapPassword);
  console.log('desktop core single-instance smoke passed');
} finally {
  if (first && first.exitCode === null) {
    first.kill();
  }
  cleanupSmokeEnvironment(smoke);
}

function waitForOutput(predicate, timeoutMilliseconds, message) {
  return new Promise((resolvePromise, reject) => {
    const startedAt = Date.now();
    const interval = setInterval(() => {
      if (predicate()) {
        clearInterval(interval);
        resolvePromise();
      } else if (Date.now() - startedAt >= timeoutMilliseconds) {
        clearInterval(interval);
        reject(new Error(message));
      }
    }, 25);
  });
}

function waitForExit(child, label) {
  return new Promise((resolvePromise, reject) => {
    const timeout = setTimeout(() => {
      child.kill();
      reject(new Error(`${label} did not exit within 45 seconds`));
    }, 45_000);
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
