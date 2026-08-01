import assert from "node:assert/strict";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { auditRelease, validateReleasePath, validateSensitiveContent } from "./audit-release.mjs";
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

test("release metadata is synchronized and updater installation is disabled", async () => {
  const metadata = await verifyReleaseMetadata(repositoryDirectory);
  assert.equal(metadata.version, "0.1.0");
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
      "CyberStrikeAI-Desktop-0.1.0-macos-arm64-portable.zip",
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
