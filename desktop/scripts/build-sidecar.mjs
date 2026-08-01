import { execFileSync, spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { binaryNamesForTarget } from './build-config.mjs';

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
  throw new Error(`unsupported desktop target: ${targetTriple}`);
}
const binaryNames = binaryNamesForTarget(targetTriple, repositoryDirectory);

const binaryDirectory = resolve(desktopDirectory, 'src-tauri', 'binaries');
const goBinary = process.env.CYBERSTRIKE_DESKTOP_GO || 'go';
const buildEnvironment = {
  ...process.env,
  CGO_ENABLED: '1',
  GOOS: target.goos,
  GOARCH: target.goarch
};
if (process.env.CYBERSTRIKE_DESKTOP_GO) {
  delete buildEnvironment.GOROOT;
}
mkdirSync(binaryDirectory, { recursive: true });

for (const binary of [
  { name: binaryNames.core, package: './cmd/desktop-core' },
  { name: binaryNames.nativeHost, package: './cmd/desktop-native-host' },
]) {
  const output = resolve(binaryDirectory, `${binary.name}-${targetTriple}${target.extension}`);
  const result = spawnSync(
    goBinary,
    ['build', '-trimpath', '-o', output, binary.package],
    {
      cwd: repositoryDirectory,
      env: buildEnvironment,
      stdio: 'inherit'
    }
  );
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
  console.log(`built desktop sidecar: ${output}`);
}
