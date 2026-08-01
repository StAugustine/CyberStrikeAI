import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments, requireReleaseTarget } from "./release-support.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");

export async function verifyPortableRuntime({
  targetTriple,
  releaseDirectory,
  extractionDirectory,
  runtimeDirectory,
  root = desktopDirectory,
}) {
  const releaseTarget = requireReleaseTarget(targetTriple);
  const packageJSON = JSON.parse(await readFile(path.join(root, "package.json"), "utf8"));
  const archivePath = await onlyFile(releaseDirectory, (name) => name.toLowerCase().endsWith("-portable.zip"));
  const archiveEntries = listArchive(archivePath);
  validateArchiveEntries(archiveEntries);
  await prepareEmptyDirectory(extractionDirectory, "portable extraction");
  await prepareEmptyDirectory(runtimeDirectory, "portable runtime");

  extractArchive(archivePath, extractionDirectory);
  let layout = await resolveRuntimeLayout(extractionDirectory, releaseTarget.portableKind);
  const expectedArchitecture = targetTriple.startsWith("aarch64") ? "arm64" : "x86_64";
  for (const binary of [layout.application, layout.sidecar, layout.nativeHost]) {
    const architecture = await binaryArchitecture(binary);
    if (architecture !== expectedArchitecture) {
      throw new Error(`${path.basename(binary)} architecture is ${architecture}, expected ${expectedArchitecture}`);
    }
  }
  const first = runMaintenance(layout.sidecar, layout.resources, runtimeDirectory, packageJSON.version);
  const restoreFixture = await createPortableRestoreFixture(runtimeDirectory, packageJSON.version);
  await writeFile(restoreFixture.liveMarker, "portable restore mutation\n", "utf8");
  const restored = runMaintenance(
    layout.sidecar,
    layout.resources,
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
  const restoredCatalog = runMaintenance(layout.sidecar, layout.resources, runtimeDirectory, packageJSON.version);
  const rollback = restoredCatalog.backups?.find((backup) => backup.id === restored.restore.rollback_backup_id);
  if (!rollback?.valid) {
    throw new Error("packaged sidecar did not retain a verified pre-restore recovery point");
  }
  const markerPath = path.join(runtimeDirectory, "data", "portable-data-marker.txt");
  await writeFile(markerPath, "portable data survives program replacement\n", "utf8");

  await rm(layout.portableRoot, { recursive: true, force: false });
  if ((await readFile(markerPath, "utf8")).trim() !== "portable data survives program replacement") {
    throw new Error("portable user data marker changed after program removal");
  }
  extractArchive(archivePath, extractionDirectory);
  layout = await resolveRuntimeLayout(extractionDirectory, releaseTarget.portableKind);
  const second = runMaintenance(layout.sidecar, layout.resources, runtimeDirectory, packageJSON.version);
  if ((await readFile(markerPath, "utf8")).trim() !== "portable data survives program replacement") {
    throw new Error("portable user data marker changed after program replacement");
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
      firstExtractMaintenance: first.operation,
      packagedBackupRestore: restored.restore.backup_id,
      preRestoreRecoveryPoint: "verified",
      programDirectoryRemoval: "passed",
      externalUserDataPreserved: "passed",
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
  const command = process.platform === "darwin" ? "/usr/bin/ditto" : "tar.exe";
  const args = process.platform === "darwin"
    ? ["-x", "-k", archivePath, destination]
    : ["-xf", archivePath, "-C", destination];
  ensureCommand(spawnSync(command, args, { encoding: "utf8", maxBuffer: 16 * 1024 * 1024 }), "extract portable archive");
}

async function resolveRuntimeLayout(extractionDirectory, kind) {
  const portableRoot = await onlyDirectory(extractionDirectory);
  if (kind === "windows-directory") {
    return {
      portableRoot,
      application: path.join(portableRoot, "CyberStrikeAI Desktop.exe"),
      sidecar: path.join(portableRoot, "cyberstrike-core.exe"),
      nativeHost: path.join(portableRoot, "cyberstrike-native-host.exe"),
      resources: path.join(portableRoot, "defaults"),
    };
  }
  const appBundle = await onlyDirectory(portableRoot, (name) => name.endsWith(".app"));
  return {
    portableRoot,
    application: path.join(appBundle, "Contents", "MacOS", "cyberstrike-desktop"),
    sidecar: path.join(appBundle, "Contents", "MacOS", "cyberstrike-core"),
    nativeHost: path.join(appBundle, "Contents", "MacOS", "cyberstrike-native-host"),
    resources: path.join(appBundle, "Contents", "Resources", "defaults"),
  };
}

function runMaintenance(
  sidecar,
  resources,
  runtimeDirectory,
  version,
  operation = "list-backups",
  backupID = "",
) {
  const argumentsList = [
    "--data-dir", path.join(runtimeDirectory, "data"),
    "--config-dir", path.join(runtimeDirectory, "config"),
    "--cache-dir", path.join(runtimeDirectory, "cache"),
    "--log-dir", path.join(runtimeDirectory, "logs"),
    "--temp-dir", path.join(runtimeDirectory, "temp"),
    "--resource-dir", resources,
    "--app-version", version,
    "--maintenance", operation,
  ];
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
