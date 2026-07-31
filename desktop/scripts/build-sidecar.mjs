import { execFileSync, spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
const desktopDirectory = resolve(scriptDirectory, '..');
const repositoryDirectory = resolve(desktopDirectory, '..');
const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync('rustc', ['--print', 'host-tuple'], { encoding: 'utf8' }).trim();

const targets = {
  'aarch64-apple-darwin': { goos: 'darwin', goarch: 'arm64', extension: '' },
  'x86_64-apple-darwin': { goos: 'darwin', goarch: 'amd64', extension: '' },
  'x86_64-pc-windows-msvc': { goos: 'windows', goarch: 'amd64', extension: '.exe' }
};
const target = targets[targetTriple];
if (!target) {
  throw new Error(`unsupported desktop PoC target: ${targetTriple}`);
}

const binaryDirectory = resolve(desktopDirectory, 'src-tauri', 'binaries');
const output = resolve(binaryDirectory, `cyberstrike-go-poc-${targetTriple}${target.extension}`);
mkdirSync(binaryDirectory, { recursive: true });

const result = spawnSync(
  'go',
  ['build', '-trimpath', '-o', output, './cmd/desktop-poc-sidecar'],
  {
    cwd: repositoryDirectory,
    env: {
      ...process.env,
      CGO_ENABLED: '0',
      GOOS: target.goos,
      GOARCH: target.goarch
    },
    stdio: 'inherit'
  }
);
if (result.error) {
  throw result.error;
}
if (result.status !== 0) {
  process.exit(result.status ?? 1);
}

console.log(`built desktop PoC sidecar: ${output}`);
