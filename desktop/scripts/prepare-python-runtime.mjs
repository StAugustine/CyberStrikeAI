import { createHash } from "node:crypto";
import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments, sha256File } from "./release-support.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");
const windowsTarget = "x86_64-pc-windows-msvc";

export async function loadPythonRuntimeConfig(root = repositoryDirectory) {
  const configPath = path.join(root, "desktop", "python-runtime.json");
  return validatePythonRuntimeConfig(JSON.parse(await readFile(configPath, "utf8")));
}

export function validatePythonRuntimeConfig(config) {
  const allowed = new Set([
    "schema_version",
    "target",
    "python_version",
    "archive_url",
    "archive_sha256",
    "lock_file",
    "required_imports",
  ]);
  if (!config || typeof config !== "object" || Array.isArray(config)) {
    throw new Error("Python runtime configuration must be an object");
  }
  for (const key of Object.keys(config)) {
    if (!allowed.has(key)) throw new Error(`unsupported Python runtime configuration field: ${key}`);
  }
  if (config.schema_version !== 1) throw new Error("unsupported Python runtime configuration version");
  if (config.target !== windowsTarget) throw new Error(`unsupported Python runtime target: ${config.target}`);
  if (!/^3\.12\.\d+$/.test(config.python_version || "")) {
    throw new Error("Python runtime version must be a pinned Python 3.12 patch release");
  }
  const expectedArchive = `python-${config.python_version}-embed-amd64.zip`;
  const archiveURL = new URL(config.archive_url);
  if (archiveURL.protocol !== "https:" || archiveURL.hostname !== "www.python.org"
    || path.posix.basename(archiveURL.pathname) !== expectedArchive) {
    throw new Error("Python runtime archive must be the pinned python.org Windows x64 embeddable ZIP");
  }
  if (!/^[a-f0-9]{64}$/.test(config.archive_sha256 || "")) {
    throw new Error("Python runtime archive SHA-256 is invalid");
  }
  if (config.lock_file !== "../requirements-win-x64.lock") {
    throw new Error("Python runtime lock file must be requirements-win-x64.lock");
  }
  if (!Array.isArray(config.required_imports) || config.required_imports.length === 0
    || config.required_imports.some((name) => !/^[a-z][a-z0-9_]*$/.test(name))) {
    throw new Error("Python runtime required imports are invalid");
  }
  if (new Set(config.required_imports).size !== config.required_imports.length) {
    throw new Error("Python runtime required imports must be unique");
  }
  return Object.freeze({ ...config, required_imports: Object.freeze([...config.required_imports]) });
}

export async function preparePythonRuntime({
  root = repositoryDirectory,
  outputDirectory,
  buildPython = process.env.CYBERSTRIKE_DESKTOP_PYTHON || "python",
}) {
  if (process.platform !== "win32") {
    throw new Error("the bundled Python runtime must be prepared on Windows");
  }
  const config = await loadPythonRuntimeConfig(root);
  const lockPath = path.resolve(root, "desktop", config.lock_file);
  const output = path.resolve(outputDirectory);
  await prepareEmptyDirectory(output);

  const archivePath = path.join(path.dirname(output), path.basename(new URL(config.archive_url).pathname));
  const response = await fetch(config.archive_url, { redirect: "error" });
  if (!response.ok) throw new Error(`download Python runtime: HTTP ${response.status}`);
  const archive = Buffer.from(await response.arrayBuffer());
  const archiveHash = createHash("sha256").update(archive).digest("hex");
  if (archiveHash !== config.archive_sha256) {
    throw new Error(`Python runtime archive SHA-256 is ${archiveHash}, expected ${config.archive_sha256}`);
  }
  await writeFile(archivePath, archive);
  try {
    extractArchive(archivePath, output);
  } finally {
    await rm(archivePath, { force: true });
  }

  const [major, minor] = config.python_version.split(".");
  const pthPath = path.join(output, `python${major}${minor}._pth`);
  await writeFile(
    pthPath,
    [`python${major}${minor}.zip`, ".", "Lib", "Lib/site-packages", "import site", ""].join("\n"),
    "utf8",
  );
  const sitePackages = path.join(output, "Lib", "site-packages");
  await mkdir(sitePackages, { recursive: true });
  run(buildPython, [
    "-m", "pip", "install",
    "--disable-pip-version-check",
    "--no-cache-dir",
    "--no-compile",
    "--no-warn-script-location",
    "--require-hashes",
    "--target", sitePackages,
    "--requirement", lockPath,
  ], "install locked Python dependencies");
  await removeRuntimeCaches(sitePackages);

  const runtimeManifest = {
    schema_version: 1,
    target: config.target,
    python_version: config.python_version,
    python_executable: "python.exe",
    source: {
      url: config.archive_url,
      sha256: config.archive_sha256,
    },
    dependency_lock: {
      file: "DEPENDENCIES.lock",
      sha256: await sha256File(lockPath),
    },
    third_party_licenses: "THIRD-PARTY-LICENSES.json",
    required_imports: config.required_imports,
  };
  await writeFile(
    path.join(output, "runtime-manifest.json"),
    `${JSON.stringify(runtimeManifest, null, 2)}\n`,
    "utf8",
  );
  await writeFile(path.join(output, "DEPENDENCIES.lock"), await readFile(lockPath));
  await writeThirdPartyLicenses(output);
  verifyRuntime(output, config);
  return runtimeManifest;
}

async function writeThirdPartyLicenses(output) {
  const script = [
    "import importlib.metadata as metadata",
    "import json",
    "items = []",
    "for dist in metadata.distributions():",
    "    name = dist.metadata.get('Name') or ''",
    "    if not name: continue",
    "    files = sorted(str(item) for item in (dist.files or []) if 'license' in str(item).lower() or 'copying' in str(item).lower())",
    "    items.append({'name': name, 'version': dist.version, 'license_expression': dist.metadata.get('License-Expression') or '', 'license': dist.metadata.get('License') or '', 'license_files': files})",
    "print(json.dumps(sorted(items, key=lambda item: item['name'].lower()), ensure_ascii=False))",
  ].join("\n");
  const result = spawnSync(path.join(output, "python.exe"), ["-I", "-c", script], {
    encoding: "utf8",
    env: {
      ...process.env,
      PYTHONDONTWRITEBYTECODE: "1",
      PYTHONNOUSERSITE: "1",
      PYTHONUTF8: "1",
    },
    maxBuffer: 16 * 1024 * 1024,
  });
  ensureCommand(result, "collect bundled Python licenses");
  const licenses = JSON.parse(result.stdout.trim());
  if (!Array.isArray(licenses) || licenses.length === 0) {
    throw new Error("bundled Python license inventory is empty");
  }
  await writeFile(
    path.join(output, "THIRD-PARTY-LICENSES.json"),
    `${JSON.stringify({ schema_version: 1, packages: licenses }, null, 2)}\n`,
    "utf8",
  );
}

function extractArchive(archivePath, destination) {
  const result = spawnSync(
    "powershell.exe",
    [
      "-NoLogo",
      "-NoProfile",
      "-NonInteractive",
      "-Command",
      "Expand-Archive -LiteralPath $env:CYBERSTRIKE_PYTHON_ARCHIVE -DestinationPath $env:CYBERSTRIKE_PYTHON_DESTINATION -Force",
    ],
    {
      env: {
        ...process.env,
        CYBERSTRIKE_PYTHON_ARCHIVE: archivePath,
        CYBERSTRIKE_PYTHON_DESTINATION: destination,
      },
      stdio: "inherit",
    },
  );
  ensureCommand(result, "extract Python runtime");
}

async function removeRuntimeCaches(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const entryPath = path.join(directory, entry.name);
    if (entry.isDirectory() && ["__pycache__", "test", "tests", "SelfTest"].includes(entry.name)) {
      await rm(entryPath, { recursive: true, force: true });
    } else if (entry.isDirectory()) {
      await removeRuntimeCaches(entryPath);
    } else if (entry.isFile() && entry.name.endsWith(".pyc")) {
      await rm(entryPath, { force: true });
    }
  }
}

function verifyRuntime(output, config) {
  const script = [
    "import importlib",
    "import json",
    "import platform",
    `required = ${JSON.stringify(config.required_imports)}`,
    "[importlib.import_module(name) for name in required]",
    "print(json.dumps({'version': platform.python_version(), 'machine': platform.machine().lower(), 'imports': required}))",
  ].join("; ");
  const result = spawnSync(path.join(output, "python.exe"), ["-I", "-c", script], {
    encoding: "utf8",
    env: {
      ...process.env,
      PYTHONDONTWRITEBYTECODE: "1",
      PYTHONNOUSERSITE: "1",
      PYTHONUTF8: "1",
    },
    maxBuffer: 16 * 1024 * 1024,
  });
  ensureCommand(result, "verify bundled Python runtime");
  const report = JSON.parse(result.stdout.trim());
  if (report.version !== config.python_version || !["amd64", "x86_64"].includes(report.machine)) {
    throw new Error(`bundled Python reported ${report.version} ${report.machine}`);
  }
}

async function prepareEmptyDirectory(directory) {
  await mkdir(directory, { recursive: true });
  if ((await readdir(directory)).length !== 0) {
    throw new Error(`Python runtime output directory must be empty: ${directory}`);
  }
}

function run(command, args, label) {
  const result = spawnSync(command, args, { cwd: repositoryDirectory, stdio: "inherit" });
  ensureCommand(result, label);
}

function ensureCommand(result, label) {
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${label} failed with status ${result.status}`);
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const args = parseArguments(process.argv.slice(2), ["output"]);
  const outputDirectory = path.resolve(args.output || process.env.CYBERSTRIKE_PYTHON_RUNTIME || "");
  if (!args.output && !process.env.CYBERSTRIKE_PYTHON_RUNTIME) {
    throw new Error("--output or CYBERSTRIKE_PYTHON_RUNTIME is required");
  }
  const manifest = await preparePythonRuntime({ outputDirectory });
  console.log(`prepared Python ${manifest.python_version} runtime: ${outputDirectory}`);
}
