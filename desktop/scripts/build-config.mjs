import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");
const windowsTarget = "x86_64-pc-windows-msvc";
const defaultNames = Object.freeze({
  core: "cyberstrike-core",
  nativeHost: "cyberstrike-native-host",
});
const allowedKeys = new Set([
  "schema_version",
  "core_binary_basename",
  "native_host_binary_basename",
]);

export function loadBuildConfig(root = repositoryDirectory) {
  const configPath = path.join(root, "desktop", "build-config.json");
  const config = JSON.parse(readFileSync(configPath, "utf8"));
  return validateBuildConfig(config);
}

export function validateBuildConfig(config) {
  if (!config || typeof config !== "object" || Array.isArray(config)) {
    throw new Error("desktop build configuration must be an object");
  }
  for (const key of Object.keys(config)) {
    if (!allowedKeys.has(key)) throw new Error(`unsupported desktop build configuration field: ${key}`);
  }
  if (config.schema_version !== 1) {
    throw new Error(`unsupported desktop build configuration version: ${config.schema_version}`);
  }
  const core = validateBinaryBasename(config.core_binary_basename, "core_binary_basename");
  const nativeHost = validateBinaryBasename(
    config.native_host_binary_basename,
    "native_host_binary_basename",
  );
  if (core.toLowerCase() === nativeHost.toLowerCase()) {
    throw new Error("desktop core and native host binary names must be different");
  }
  return Object.freeze({ core, nativeHost });
}

export function binaryNamesForTarget(targetTriple, root = repositoryDirectory) {
  if (targetTriple === windowsTarget) return loadBuildConfig(root);
  return defaultNames;
}

export function writeTauriBuildConfig(targetTriple, root = repositoryDirectory) {
  const config = tauriBuildConfigForTarget(targetTriple, root);
  const outputDirectory = path.join(root, ".tmp", "desktop-tauri-config", targetTriple);
  const outputPath = path.join(outputDirectory, "tauri.conf.json");
  mkdirSync(outputDirectory, { recursive: true });
  writeFileSync(
    outputPath,
    `${JSON.stringify(config, null, 2)}\n`,
    "utf8",
  );
  return outputPath;
}

export function tauriBuildConfigForTarget(targetTriple, root = repositoryDirectory) {
  const names = binaryNamesForTarget(targetTriple, root);
  return {
    bundle: {
      externalBin: [
        `binaries/${names.core}`,
        `binaries/${names.nativeHost}`,
      ],
    },
  };
}

function validateBinaryBasename(value, field) {
  if (typeof value !== "string" || value.length === 0 || value.length > 64) {
    throw new Error(`${field} must contain between 1 and 64 characters`);
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(value) || value.endsWith(".")) {
    throw new Error(`${field} must be a safe executable basename`);
  }
  if (value.toLowerCase().endsWith(".exe")) {
    throw new Error(`${field} must not include the .exe extension`);
  }
  const reserved = value.split(".", 1)[0].toUpperCase();
  if (/^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])$/.test(reserved)) {
    throw new Error(`${field} uses a reserved Windows filename`);
  }
  if (value.toLowerCase() === "cyberstrike-desktop") {
    throw new Error(`${field} conflicts with the desktop application executable`);
  }
  return value;
}
