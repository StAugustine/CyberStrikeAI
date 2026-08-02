import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments, requireReleaseTarget } from "./release-support.mjs";
import { binaryNamesForTarget } from "./build-config.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const r1Version = "0.1.0";

export async function verifyPortableRuntime({
  targetTriple,
  releaseDirectory,
  extractionDirectory,
  runtimeDirectory,
  root = desktopDirectory,
}) {
  const releaseTarget = requireReleaseTarget(targetTriple);
  const binaryNames = binaryNamesForTarget(targetTriple, path.resolve(root, ".."));
  const packageJSON = JSON.parse(await readFile(path.join(root, "package.json"), "utf8"));
  const archivePath = await onlyFile(releaseDirectory, (name) => name.toLowerCase().endsWith("-portable.zip"));
  const archiveEntries = listArchive(archivePath);
  validateArchiveEntries(archiveEntries);
  await prepareEmptyDirectory(extractionDirectory, "portable extraction");
  await prepareEmptyDirectory(runtimeDirectory, "portable runtime");

  extractArchive(archivePath, extractionDirectory);
  let layout = await resolveRuntimeLayout(extractionDirectory, releaseTarget.portableKind, binaryNames);
  const expectedArchitecture = targetTriple.startsWith("aarch64") ? "arm64" : "x86_64";
  for (const binary of [layout.application, layout.sidecar, layout.nativeHost]) {
    const architecture = await binaryArchitecture(binary);
    if (architecture !== expectedArchitecture) {
      throw new Error(`${path.basename(binary)} architecture is ${architecture}, expected ${expectedArchitecture}`);
    }
  }
  let windowsSubsystems;
  let bundledPython;
  if (releaseTarget.portableKind === "windows-directory") {
    windowsSubsystems = {};
    for (const [label, binary] of [
      ["application", layout.application],
      ["sidecar", layout.sidecar],
      ["nativeHost", layout.nativeHost],
    ]) {
      const subsystem = await windowsPESubsystem(binary);
      if (subsystem !== 2) {
        throw new Error(`${path.basename(binary)} PE subsystem is ${subsystem}, expected Windows GUI (2)`);
      }
      windowsSubsystems[label] = "windows-gui";
    }
    const pythonArchitecture = await binaryArchitecture(layout.pythonExecutable);
    if (pythonArchitecture !== expectedArchitecture) {
      throw new Error(`python.exe architecture is ${pythonArchitecture}, expected ${expectedArchitecture}`);
    }
    bundledPython = await verifyBundledPython(layout.pythonRuntime, layout.pythonExecutable);
  }
  const upgradeFixture = await createPortableR1Fixture(runtimeDirectory, layout.resources);
  const upgradedLifecycle = runCoreLifecycle(
    layout.sidecar,
    layout.resources,
    layout.pythonRuntime,
    runtimeDirectory,
    packageJSON.version,
    true,
  );
  await verifyPortableR1Fixture(upgradeFixture, packageJSON.version);
  const first = runMaintenance(
    layout.sidecar,
    layout.resources,
    layout.pythonRuntime,
    runtimeDirectory,
    packageJSON.version,
  );
  const upgradeBackup = first.backups?.find((backup) => backup.valid
    && backup.kind === "upgrade"
    && backup.from_version === r1Version
    && backup.to_version === packageJSON.version);
  if (!upgradeBackup) {
    throw new Error(`packaged sidecar did not create an ${r1Version} to ${packageJSON.version} recovery point`);
  }
  const markerPath = path.join(runtimeDirectory, "data", "portable-data-marker.txt");
  await writeFile(markerPath, "portable data survives program replacement\n", "utf8");

  await rm(layout.portableRoot, { recursive: true, force: false });
  if ((await readFile(markerPath, "utf8")).trim() !== "portable data survives program replacement") {
    throw new Error("portable user data marker changed after program removal");
  }
  extractArchive(archivePath, extractionDirectory);
  layout = await resolveRuntimeLayout(extractionDirectory, releaseTarget.portableKind, binaryNames);
  const replacedLifecycle = runCoreLifecycle(
    layout.sidecar,
    layout.resources,
    layout.pythonRuntime,
    runtimeDirectory,
    packageJSON.version,
    false,
  );
  const second = runMaintenance(
    layout.sidecar,
    layout.resources,
    layout.pythonRuntime,
    runtimeDirectory,
    packageJSON.version,
  );
  if ((await readFile(markerPath, "utf8")).trim() !== "portable data survives program replacement") {
    throw new Error("portable user data marker changed after program replacement");
  }
  const restoreFixture = await createPortableRestoreFixture(runtimeDirectory, packageJSON.version);
  await writeFile(restoreFixture.liveMarker, "portable restore mutation\n", "utf8");
  const restored = runMaintenance(
    layout.sidecar,
    layout.resources,
    layout.pythonRuntime,
    runtimeDirectory,
    packageJSON.version,
    "restore-backup",
    restoreFixture.backupID,
  );
  if (restored.restore?.backup_id !== restoreFixture.backupID
    || !restored.restore?.rollback_backup_id) {
    throw new Error("packaged sidecar returned an invalid restore response");
  }
  if ((await readFile(restoreFixture.liveMarker, "utf8")) !== restoreFixture.expectedContent) {
    throw new Error("packaged sidecar did not restore the verified portable fixture");
  }
  const restoredCatalog = runMaintenance(
    layout.sidecar,
    layout.resources,
    layout.pythonRuntime,
    runtimeDirectory,
    packageJSON.version,
  );
  const rollback = restoredCatalog.backups?.find((backup) => backup.id === restored.restore.rollback_backup_id);
  if (!rollback?.valid) {
    throw new Error("packaged sidecar did not retain a verified pre-restore recovery point");
  }

  const report = {
    schemaVersion: 1,
    product: "CyberStrikeAI Desktop",
    version: packageJSON.version,
    target: targetTriple,
    format: "portable-zip",
    result: "passed",
    checks: {
      safeArchivePaths: archiveEntries.length,
      applicationArchitecture: expectedArchitecture,
      sidecarArchitecture: expectedArchitecture,
      nativeHostArchitecture: expectedArchitecture,
      ...(windowsSubsystems ? { windowsSubsystems } : {}),
      ...(bundledPython ? { bundledPython } : {}),
      r1VersionFixture: r1Version,
      r1ToR2Lifecycle: upgradedLifecycle,
      r1ToR2RecoveryPoint: upgradeBackup.id,
      r1ConfigurationPreserved: "passed",
      firstExtractMaintenance: first.operation,
      packagedBackupRestore: restored.restore.backup_id,
      preRestoreRecoveryPoint: "verified",
      programDirectoryRemoval: "passed",
      externalUserDataPreserved: "passed",
      replacementLifecycle: replacedLifecycle,
      replacementExtractMaintenance: second.operation,
    },
  };
  await writeFile(
    path.join(releaseDirectory, "portable-runtime-report.json"),
    `${JSON.stringify(report, null, 2)}\n`,
    "utf8",
  );
  return report;
}

function runCoreLifecycle(
  sidecar,
  resources,
  pythonRuntime,
  runtimeDirectory,
  version,
  bootstrapRequired,
) {
  const commands = [];
  if (bootstrapRequired) {
    commands.push({
      type: "BOOTSTRAP",
      protocol_version: 1,
      password: "portable-runtime-bootstrap",
    });
  }
  commands.push({ type: "SHUTDOWN", protocol_version: 1 });
  const result = spawnSync(
    sidecar,
    sidecarArguments(resources, pythonRuntime, runtimeDirectory, version),
    {
      cwd: path.dirname(sidecar),
      encoding: "utf8",
      input: `${commands.map((command) => JSON.stringify(command)).join("\n")}\n`,
      timeout: 60_000,
      maxBuffer: 16 * 1024 * 1024,
    },
  );
  ensureCommand(result, "run packaged sidecar lifecycle");
  const messages = result.stdout.split(/\r?\n/).filter(Boolean).map((line) => JSON.parse(line));
  const types = messages.map((message) => message.type);
  if (!messages.some((message) => message.type === "READY" && message.app_version === version)) {
    throw new Error("packaged sidecar did not reach READY with the release version");
  }
  if (bootstrapRequired && !types.includes("BOOTSTRAP_REQUIRED")) {
    throw new Error("R1 fixture did not preserve the expected first-start bootstrap state");
  }
  if (!bootstrapRequired && (types.includes("BOOTSTRAP_REQUIRED") || types.includes("CREDENTIAL_MIGRATION_REQUIRED"))) {
    throw new Error("program replacement unexpectedly requested desktop reinitialization");
  }
  return bootstrapRequired ? "BOOTSTRAP_REQUIRED to READY" : "READY without reinitialization";
}

function sidecarArguments(resources, pythonRuntime, runtimeDirectory, version) {
  const argumentsList = [
    "--data-dir", path.join(runtimeDirectory, "data"),
    "--config-dir", path.join(runtimeDirectory, "config"),
    "--cache-dir", path.join(runtimeDirectory, "cache"),
    "--log-dir", path.join(runtimeDirectory, "logs"),
    "--temp-dir", path.join(runtimeDirectory, "temp"),
    "--resource-dir", resources,
    "--app-version", version,
  ];
  if (pythonRuntime) {
    argumentsList.push(
      "--python-runtime-dir", pythonRuntime,
      "--python-executable", path.join(pythonRuntime, "python.exe"),
    );
  }
  return argumentsList;
}

async function createPortableR1Fixture(runtimeDirectory, resources) {
  const dataMarker = path.join(runtimeDirectory, "data", "portable-data-marker.txt");
  const configFile = path.join(runtimeDirectory, "config", "config.yaml");
  const resourceState = path.join(runtimeDirectory, "data", "resource-state.json");
  const dataContent = "portable R1 data survives R2 replacement\n";
  const configMarker = "# portable-r1-configuration-marker\n";
  const template = await readFile(path.join(resources, "config.example.yaml"), "utf8");
  await mkdir(path.dirname(dataMarker), { recursive: true });
  await mkdir(path.dirname(configFile), { recursive: true });
  await writeFile(dataMarker, dataContent, { encoding: "utf8", mode: 0o600 });
  await writeFile(configFile, `${configMarker}${template}`, { encoding: "utf8", mode: 0o600 });
  await writeFile(
    resourceState,
    `${JSON.stringify({ schema_version: 1, app_version: r1Version, files: {} }, null, 2)}\n`,
    { encoding: "utf8", mode: 0o600 },
  );
  return { dataMarker, dataContent, configFile, configMarker, resourceState };
}

async function verifyPortableR1Fixture(fixture, version) {
  if ((await readFile(fixture.dataMarker, "utf8")) !== fixture.dataContent) {
    throw new Error("R1 data marker changed during the R2 upgrade");
  }
  if (!(await readFile(fixture.configFile, "utf8")).startsWith(fixture.configMarker)) {
    throw new Error("R1 configuration changed during the R2 upgrade");
  }
  const state = JSON.parse(await readFile(fixture.resourceState, "utf8"));
  if (state.app_version !== version) {
    throw new Error(`desktop resource state is ${state.app_version}, expected ${version}`);
  }
}

export async function binaryArchitecture(filePath) {
  const data = await readFile(filePath);
  if (data.length >= 64 && data[0] === 0x4d && data[1] === 0x5a) {
    const peOffset = data.readUInt32LE(0x3c);
    if (peOffset + 6 > data.length || data.toString("ascii", peOffset, peOffset + 4) !== "PE\0\0") {
      throw new Error(`invalid PE executable: ${filePath}`);
    }
    const machine = data.readUInt16LE(peOffset + 4);
    if (machine === 0x8664) return "x86_64";
    if (machine === 0xaa64) return "arm64";
    throw new Error(`unsupported PE machine 0x${machine.toString(16)}: ${filePath}`);
  }
  if (data.length >= 8 && data.readUInt32LE(0) === 0xfeedfacf) {
    const cpuType = data.readUInt32LE(4);
    if (cpuType === 0x01000007) return "x86_64";
    if (cpuType === 0x0100000c) return "arm64";
    throw new Error(`unsupported Mach-O CPU 0x${cpuType.toString(16)}: ${filePath}`);
  }
  throw new Error(`unsupported executable format: ${filePath}`);
}

export async function windowsPESubsystem(filePath) {
  return parseWindowsPESubsystem(await readFile(filePath), filePath);
}

export function parseWindowsPESubsystem(data, filePath = "<buffer>") {
  if (data.length < 64 || data[0] !== 0x4d || data[1] !== 0x5a) {
    throw new Error(`invalid PE executable: ${filePath}`);
  }
  const peOffset = data.readUInt32LE(0x3c);
  const optionalHeaderOffset = peOffset + 24;
  const optionalHeaderSize = peOffset + 22 <= data.length ? data.readUInt16LE(peOffset + 20) : 0;
  if (optionalHeaderSize < 70 || optionalHeaderOffset + 70 > data.length
    || data.toString("ascii", peOffset, peOffset + 4) !== "PE\0\0") {
    throw new Error(`invalid PE optional header: ${filePath}`);
  }
  const magic = data.readUInt16LE(optionalHeaderOffset);
  if (magic !== 0x10b && magic !== 0x20b) {
    throw new Error(`unsupported PE optional header 0x${magic.toString(16)}: ${filePath}`);
  }
  return data.readUInt16LE(optionalHeaderOffset + 68);
}

export function validateArchiveEntries(entries) {
  if (entries.length === 0) throw new Error("portable archive is empty");
  for (const entry of entries) {
    const normalized = entry.replaceAll("\\", "/");
    if (!normalized || normalized.startsWith("/") || /^[A-Za-z]:/.test(normalized)
      || normalized.split("/").includes("..") || normalized.includes("\0")) {
      throw new Error(`unsafe portable archive path: ${entry}`);
    }
  }
}

function listArchive(archivePath) {
  const command = process.platform === "darwin" ? "/usr/bin/unzip" : "tar.exe";
  const args = process.platform === "darwin" ? ["-Z1", archivePath] : ["-tf", archivePath];
  const result = spawnSync(command, args, { encoding: "utf8", maxBuffer: 16 * 1024 * 1024 });
  ensureCommand(result, "list portable archive");
  return result.stdout.split(/\r?\n/).filter(Boolean);
}

function extractArchive(archivePath, destination) {
  if (process.platform === "darwin") {
    ensureCommand(
      spawnSync("/usr/bin/ditto", ["-x", "-k", archivePath, destination], {
        encoding: "utf8",
        maxBuffer: 16 * 1024 * 1024,
      }),
      "extract portable archive",
    );
    return;
  }
  ensureCommand(
    spawnSync(
      "powershell.exe",
      [
        "-NoLogo",
        "-NoProfile",
        "-NonInteractive",
        "-Command",
        "Add-Type -AssemblyName System.IO.Compression.FileSystem; [System.IO.Compression.ZipFile]::ExtractToDirectory($env:CYBERSTRIKE_ZIP_ARCHIVE, $env:CYBERSTRIKE_ZIP_DESTINATION)",
      ],
      {
        encoding: "utf8",
        env: {
          ...process.env,
          CYBERSTRIKE_ZIP_ARCHIVE: archivePath,
          CYBERSTRIKE_ZIP_DESTINATION: destination,
        },
        maxBuffer: 16 * 1024 * 1024,
      },
    ),
    "extract portable archive",
  );
}

async function resolveRuntimeLayout(extractionDirectory, kind, binaryNames) {
  const portableRoot = await onlyDirectory(extractionDirectory);
  if (kind === "windows-directory") {
    return {
      portableRoot,
      application: path.join(portableRoot, "CyberStrikeAI Desktop.exe"),
      sidecar: path.join(portableRoot, `${binaryNames.core}.exe`),
      nativeHost: path.join(portableRoot, `${binaryNames.nativeHost}.exe`),
      resources: path.join(portableRoot, "defaults"),
      pythonRuntime: path.join(portableRoot, "runtime", "python"),
      pythonExecutable: path.join(portableRoot, "runtime", "python", "python.exe"),
    };
  }
  const appBundle = await onlyDirectory(portableRoot, (name) => name.endsWith(".app"));
  return {
    portableRoot,
    application: path.join(appBundle, "Contents", "MacOS", "cyberstrike-desktop"),
    sidecar: path.join(appBundle, "Contents", "MacOS", binaryNames.core),
    nativeHost: path.join(appBundle, "Contents", "MacOS", binaryNames.nativeHost),
    resources: path.join(appBundle, "Contents", "Resources", "defaults"),
    pythonRuntime: undefined,
    pythonExecutable: undefined,
  };
}

async function verifyBundledPython(runtimeDirectory, executable) {
  const manifest = JSON.parse(
    await readFile(path.join(runtimeDirectory, "runtime-manifest.json"), "utf8"),
  );
  const script = [
    "import importlib",
    "import json",
    "import platform",
    `required = ${JSON.stringify(manifest.required_imports)}`,
    "[importlib.import_module(name) for name in required]",
    "print(json.dumps({'version': platform.python_version(), 'imports': required}))",
  ].join("; ");
  const result = spawnSync(executable, ["-I", "-c", script], {
    cwd: runtimeDirectory,
    encoding: "utf8",
    env: {
      ...process.env,
      PYTHONDONTWRITEBYTECODE: "1",
      PYTHONNOUSERSITE: "1",
      PYTHONUTF8: "1",
    },
    timeout: 60_000,
    maxBuffer: 16 * 1024 * 1024,
  });
  ensureCommand(result, "run bundled Python import smoke");
  const report = JSON.parse(result.stdout.trim());
  if (report.version !== manifest.python_version
    || JSON.stringify(report.imports) !== JSON.stringify(manifest.required_imports)) {
    throw new Error("bundled Python import smoke returned unexpected metadata");
  }
  return {
    version: report.version,
    architecture: "x86_64",
    requiredImports: report.imports,
  };
}

function runMaintenance(
  sidecar,
  resources,
  pythonRuntime,
  runtimeDirectory,
  version,
  operation = "list-backups",
  backupID = "",
) {
  const argumentsList = sidecarArguments(resources, pythonRuntime, runtimeDirectory, version);
  argumentsList.push("--maintenance", operation);
  if (backupID) argumentsList.push("--backup-id", backupID);
  const result = spawnSync(sidecar, argumentsList, {
    cwd: path.dirname(sidecar),
    encoding: "utf8",
    timeout: 60_000,
    maxBuffer: 16 * 1024 * 1024,
  });
  ensureCommand(result, "run packaged sidecar maintenance");
  const response = JSON.parse(result.stdout);
  if (response.operation !== operation
    || (operation === "list-backups" && response.backups !== undefined && !Array.isArray(response.backups))) {
    throw new Error("packaged sidecar returned an invalid maintenance response");
  }
  return response;
}

async function createPortableRestoreFixture(runtimeDirectory, version) {
  const backupID = "portable-restore-fixture";
  const expectedContent = "portable restore verified\n";
  const logicalPath = "data/portable-restore-marker.txt";
  const backupDirectory = path.join(runtimeDirectory, "data", "backups", backupID);
  const payloadPath = path.join(backupDirectory, "payload", "data", "portable-restore-marker.txt");
  const liveMarker = path.join(runtimeDirectory, "data", "portable-restore-marker.txt");
  await mkdir(path.dirname(payloadPath), { recursive: true });
  await writeFile(payloadPath, expectedContent, { encoding: "utf8", mode: 0o600 });
  await writeFile(liveMarker, expectedContent, { encoding: "utf8", mode: 0o600 });
  const size = Buffer.byteLength(expectedContent);
  const manifest = {
    schema_version: 1,
    id: backupID,
    kind: "upgrade",
    from_version: version,
    to_version: version,
    created_at: "2026-07-31T00:00:00Z",
    total_bytes: size,
    files: [{
      path: logicalPath,
      kind: "file",
      sha256: createHash("sha256").update(expectedContent).digest("hex"),
      size,
      mode: 0o600,
    }],
  };
  await writeFile(
    path.join(backupDirectory, "manifest.json"),
    `${JSON.stringify(manifest, null, 2)}\n`,
    { encoding: "utf8", mode: 0o600 },
  );
  return { backupID, expectedContent, liveMarker };
}

function ensureCommand(result, label) {
  if (result.error) throw new Error(`${label}: ${result.error.message}`);
  if (result.status !== 0) {
    throw new Error(`${label} failed (${result.status}): ${(result.stderr || result.stdout || "").trim()}`);
  }
}

async function prepareEmptyDirectory(directory, label) {
  await mkdir(directory, { recursive: true });
  if ((await readdir(directory)).length !== 0) throw new Error(`${label} directory must be empty: ${directory}`);
}

async function onlyFile(directory, predicate) {
  const entries = (await readdir(directory, { withFileTypes: true }))
    .filter((entry) => entry.isFile() && predicate(entry.name));
  if (entries.length !== 1) throw new Error(`expected one portable archive, found ${entries.length}`);
  return path.join(directory, entries[0].name);
}

async function onlyDirectory(directory, predicate = () => true) {
  const entries = (await readdir(directory, { withFileTypes: true }))
    .filter((entry) => entry.isDirectory() && predicate(entry.name));
  if (entries.length !== 1) throw new Error(`expected one portable directory, found ${entries.length}`);
  return path.join(directory, entries[0].name);
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const args = parseArguments(process.argv.slice(2), [
    "target",
    "release-directory",
    "extraction-directory",
    "runtime-directory",
  ]);
  const targetTriple = args.target || process.env.CYBERSTRIKE_DESKTOP_TARGET;
  const releaseDirectory = path.resolve(
    args["release-directory"] || process.env.CYBERSTRIKE_RELEASE_OUTPUT || "",
  );
  const extractionDirectory = path.resolve(
    args["extraction-directory"] || process.env.CYBERSTRIKE_PORTABLE_EXTRACT || "",
  );
  const runtimeDirectory = path.resolve(
    args["runtime-directory"] || process.env.CYBERSTRIKE_PORTABLE_RUNTIME || "",
  );
  if (!targetTriple || (!args["release-directory"] && !process.env.CYBERSTRIKE_RELEASE_OUTPUT)
    || (!args["extraction-directory"] && !process.env.CYBERSTRIKE_PORTABLE_EXTRACT)
    || (!args["runtime-directory"] && !process.env.CYBERSTRIKE_PORTABLE_RUNTIME)) {
    throw new Error("target, release, extraction and runtime directories are required");
  }
  const report = await verifyPortableRuntime({
    targetTriple,
    releaseDirectory,
    extractionDirectory,
    runtimeDirectory,
  });
  console.log(`portable runtime verification passed: ${report.target}`);
}
