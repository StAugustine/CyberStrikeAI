import {
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync
} from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const smokeBootstrapPassword = 'desktop-smoke-password';

const scriptDirectory = dirname(fileURLToPath(import.meta.url));
export const desktopDirectory = resolve(scriptDirectory, '..');
export const repositoryDirectory = resolve(desktopDirectory, '..');

export function prepareSmokeEnvironment() {
  const temporaryDirectory = resolve(repositoryDirectory, '.tmp');
  mkdirSync(temporaryDirectory, { recursive: true });
  const smokeDirectory = mkdtempSync(join(temporaryDirectory, 'desktop-smoke-'));
  const resourceDirectory = resolve(smokeDirectory, 'defaults');
  const runtimeDirectory = resolve(smokeDirectory, 'runtime');
  mkdirSync(resourceDirectory, { recursive: true });
  mkdirSync(runtimeDirectory, { recursive: true });

  const manifestPath = resolve(desktopDirectory, 'resources', 'manifest.json');
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  copyFileSync(manifestPath, resolve(resourceDirectory, 'manifest.json'));
  for (const file of manifest.files) {
    const source = resolve(repositoryDirectory, file.path);
    const destination = resolve(resourceDirectory, file.path);
    mkdirSync(dirname(destination), { recursive: true });
    copyFileSync(source, destination);
  }

  return {
    directory: smokeDirectory,
    environment: {
      CYBERSTRIKE_DESKTOP_TEST_ROOT: runtimeDirectory,
      CYBERSTRIKE_DESKTOP_RESOURCE_DIR: resourceDirectory
    }
  };
}

export function cleanupSmokeEnvironment(smoke) {
  if (!smoke?.directory.startsWith(resolve(repositoryDirectory, '.tmp', 'desktop-smoke-'))) {
    throw new Error('refusing to clean an unexpected smoke directory');
  }
  rmSync(smoke.directory, { recursive: true, force: true });
}

export function assertSecretAbsent(root, secret) {
  const secretBytes = Buffer.from(secret);
  for (const file of walkFiles(root)) {
    if (readFileSync(file).includes(secretBytes)) {
      throw new Error(`bootstrap password was persisted in ${file}`);
    }
  }
}

function walkFiles(root) {
  const files = [];
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = resolve(root, entry.name);
    if (entry.isDirectory()) {
      files.push(...walkFiles(path));
    } else if (entry.isFile() && statSync(path).size <= 100 * 1024 * 1024) {
      files.push(path);
    }
  }
  return files;
}
