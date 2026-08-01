import assert from "node:assert/strict";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { auditRelease, validateReleasePath, validateSensitiveContent } from "./audit-release.mjs";
import {
  binaryNamesForTarget,
  loadBuildConfig,
  tauriBuildConfigForTarget,
  validateBuildConfig,
} from "./build-config.mjs";
import { createReleaseChecksums } from "./create-release-checksums.mjs";
import { createSBOM, parseCargoComponents, parseGoComponents, parseNPMComponents, stableSBOMDigest } from "./generate-sbom.mjs";
import { packagePortable } from "./package-portable.mjs";
import { parseArguments, requireReleaseTarget } from "./release-support.mjs";
import { verifyReleaseMetadata } from "./verify-release-metadata.mjs";
import { validateArchiveEntries } from "./verify-portable-runtime.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");
const temporaryRoot = path.join(repositoryDirectory, ".tmp", `release-tools-test-${process.pid}`);

test("release target and argument parsing fail closed", () => {
  assert.equal(requireReleaseTarget("x86_64-pc-windows-msvc").portableKind, "windows-directory");
  assert.throws(() => requireReleaseTarget("linux"), /unsupported release target/);
  assert.deepEqual(parseArguments(["--target", "aarch64-apple-darwin"], ["target"]), {
    target: "aarch64-apple-darwin",
  });
  assert.throws(() => parseArguments(["--unknown", "value"], ["target"]), /unsupported/);
});

test("Windows x64 binary names come from the validated build configuration", () => {
  assert.deepEqual(loadBuildConfig(repositoryDirectory), { core: "server", nativeHost: "sihost" });
  assert.deepEqual(binaryNamesForTarget("x86_64-pc-windows-msvc", repositoryDirectory), {
    core: "server",
    nativeHost: "sihost",
  });
  assert.deepEqual(binaryNamesForTarget("aarch64-apple-darwin", repositoryDirectory), {
    core: "cyberstrike-core",
    nativeHost: "cyberstrike-native-host",
  });
  assert.deepEqual(tauriBuildConfigForTarget("x86_64-pc-windows-msvc", repositoryDirectory), {
    bundle: { externalBin: ["binaries/server", "binaries/sihost"] },
  });
  assert.throws(
    () => validateBuildConfig({
      schema_version: 1,
      core_binary_basename: "../server",
      native_host_binary_basename: "sihost",
    }),
    /safe executable basename/,
  );
  assert.throws(
    () => validateBuildConfig({
      schema_version: 1,
      core_binary_basename: "server.exe",
      native_host_binary_basename: "sihost",
    }),
    /must not include the .exe extension/,
  );
  assert.throws(
    () => validateBuildConfig({
      schema_version: 1,
      core_binary_basename: "server",
      native_host_binary_basename: "SERVER",
    }),
    /must be different/,
  );
});

test("release metadata is synchronized and updater installation is disabled", async () => {
  const metadata = await verifyReleaseMetadata(repositoryDirectory);
  assert.equal(metadata.version, "0.2.0");
  assert.equal(metadata.browserExtensionVersion, "0.4.0");
  assert.equal(metadata.burpExtensionVersion, "1.1.0");
});

test("lock file parsers create ecosystem components", () => {
  const go = parseGoComponents("require (\n example.com/direct v1.2.3\n example.com/indirect v2.0.0 // indirect\n)\n");
  assert.equal(go.length, 2);
  assert.match(go[0].purl, /^pkg:golang\//);
  const cargo = parseCargoComponents('[[package]]\nname = "serde"\nversion = "1.0.0"\nchecksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"\n', { name: "app", version: "1" });
  assert.equal(cargo[0].hashes[0].alg, "SHA-256");
  const npm = parseNPMComponents({ packages: { "node_modules/example": { version: "1.0.0", dev: true } } });
  assert.equal(npm[0].purl, "pkg:npm/example@1.0.0");
});

test("CycloneDX SBOM is deterministic and has unique references", async () => {
  const first = await createSBOM(repositoryDirectory);
  const second = await createSBOM(repositoryDirectory);
  assert.equal(stableSBOMDigest(first), stableSBOMDigest(second));
  assert.equal(first.bomFormat, "CycloneDX");
  assert.equal(new Set(first.components.map((item) => item["bom-ref"])).size, first.components.length);
});

test("release audit rejects sensitive files and key material", () => {
  assert.throws(() => validateReleasePath("defaults/config.yaml"), /sensitive configuration/);
  assert.throws(() => validateReleasePath("data/state.sqlite"), /sensitive or temporary/);
  assert.doesNotThrow(() => validateReleasePath("defaults/config.example.yaml"));
  assert.throws(
    () => validateSensitiveContent("-----BEGIN PRIVATE KEY-----", "secret.txt"),
    /private key material/,
  );
});

test("portable archive paths reject traversal and absolute entries", () => {
  assert.doesNotThrow(() => validateArchiveEntries(["CyberStrikeAI/app.exe"]));
  assert.throws(() => validateArchiveEntries(["../escape"]), /unsafe portable archive path/);
  assert.throws(() => validateArchiveEntries(["C:\\escape.exe"]), /unsafe portable archive path/);
});

test("Windows portable packaging stages the configured executable names", async () => {
  const fixtureRoot = path.join(temporaryRoot, "windows-root");
  const buildRoot = path.join(temporaryRoot, "windows-build");
  const stageDirectory = path.join(temporaryRoot, "windows-stage");
  const outputDirectory = path.join(temporaryRoot, "windows-output");
  const targetTriple = "x86_64-pc-windows-msvc";
  await rm(temporaryRoot, { recursive: true, force: true });
  await mkdir(path.join(fixtureRoot, "desktop", "src-tauri", "binaries"), { recursive: true });
  await mkdir(buildRoot, { recursive: true });
  await writeFile(path.join(fixtureRoot, "LICENSE"), "fixture", "utf8");
  await writeFile(
    path.join(fixtureRoot, "desktop", "package.json"),
    JSON.stringify({ version: "0.2.0" }),
    "utf8",
  );
  await writeFile(
    path.join(fixtureRoot, "desktop", "build-config.json"),
    JSON.stringify({
      schema_version: 1,
      core_binary_basename: "server",
      native_host_binary_basename: "sihost",
    }),
    "utf8",
  );
  await writeFile(
    path.join(fixtureRoot, "desktop", "src-tauri", "tauri.conf.json"),
    JSON.stringify({ bundle: { resources: {} } }),
    "utf8",
  );
  await writeFile(path.join(buildRoot, "cyberstrike-desktop.exe"), "desktop", "utf8");
  await writeFile(
    path.join(fixtureRoot, "desktop", "src-tauri", "binaries", `server-${targetTriple}.exe`),
    "core",
    "utf8",
  );
  await writeFile(
    path.join(fixtureRoot, "desktop", "src-tauri", "binaries", `sihost-${targetTriple}.exe`),
    "native-host",
    "utf8",
  );

  try {
    const result = await packagePortable({
      root: fixtureRoot,
      targetTriple,
      buildRoot,
      stageDirectory,
      outputDirectory,
      archiveRunner: async ({ archivePath }) => writeFile(archivePath, "zip fixture", "utf8"),
    });
    assert.equal(await readFile(path.join(result.portableRoot, "server.exe"), "utf8"), "core");
    assert.equal(await readFile(path.join(result.portableRoot, "sihost.exe"), "utf8"), "native-host");
    await assert.rejects(readFile(path.join(result.portableRoot, "cyberstrike-core.exe")), /ENOENT/);
    await assert.rejects(
      readFile(path.join(result.portableRoot, "cyberstrike-native-host.exe")),
      /ENOENT/,
    );
  } finally {
    await rm(temporaryRoot, { recursive: true, force: true });
  }
});

test("portable archive audit and checksums cover all evidence", async () => {
  const buildRoot = path.join(temporaryRoot, "build");
  const stageDirectory = path.join(temporaryRoot, "stage");
  const outputDirectory = path.join(temporaryRoot, "output");
  await rm(temporaryRoot, { recursive: true, force: true });
  const contents = path.join(buildRoot, "bundle", "macos", "CyberStrikeAI Desktop.app", "Contents");
  await mkdir(path.join(contents, "MacOS"), { recursive: true });
  await mkdir(path.join(contents, "Resources", "defaults"), { recursive: true });
  await writeFile(path.join(contents, "Info.plist"), "fixture", "utf8");
  await writeFile(path.join(contents, "MacOS", "cyberstrike-desktop"), "fixture", "utf8");
  await writeFile(path.join(contents, "MacOS", "cyberstrike-core"), "fixture", "utf8");
  await writeFile(path.join(contents, "MacOS", "cyberstrike-native-host"), "fixture", "utf8");
  await writeFile(path.join(contents, "Resources", "defaults", "manifest.json"), "{}", "utf8");
  await writeFile(path.join(contents, "Resources", "defaults", "config.example.yaml"), "fixture", "utf8");

  try {
    await packagePortable({
      root: repositoryDirectory,
      targetTriple: "aarch64-apple-darwin",
      buildRoot,
      stageDirectory,
      outputDirectory,
      archiveRunner: async ({ archivePath }) => writeFile(archivePath, "zip fixture", "utf8"),
    });
    const report = await auditRelease({
      root: repositoryDirectory,
      targetTriple: "aarch64-apple-darwin",
      stageDirectory,
      outputDirectory,
    });
    assert.equal(report.result, "passed");
    const sbom = await createSBOM(repositoryDirectory);
    await writeFile(path.join(outputDirectory, "sbom.cdx.json"), `${JSON.stringify(sbom, null, 2)}\n`, "utf8");
    await writeFile(path.join(outputDirectory, "portable-runtime-report.json"), "{}\n", "utf8");
    const manifest = await createReleaseChecksums({
      targetTriple: "aarch64-apple-darwin",
      directory: outputDirectory,
      root: desktopDirectory,
    });
    assert.equal(manifest.unsigned, true);
    assert.equal(manifest.portable, true);
    assert.deepEqual(manifest.files.map((item) => item.name), [
      "audit-report.json",
      "CyberStrikeAI-Desktop-0.2.0-macos-arm64-portable.zip",
      "portable-runtime-report.json",
      "sbom.cdx.json",
    ]);
    const sums = await readFile(path.join(outputDirectory, "SHA256SUMS"), "utf8");
    assert.match(sums, /audit-report\.json/);
    assert.match(sums, /sbom\.cdx\.json/);
  } finally {
    await rm(temporaryRoot, { recursive: true, force: true });
  }
});
