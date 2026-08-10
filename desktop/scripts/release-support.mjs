import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";

export const releaseTargets = Object.freeze({
  "aarch64-apple-darwin": Object.freeze({
    tauriBundle: "app",
    portableKind: "macos-app",
    archiveLabel: "macos-arm64",
  }),
  "x86_64-apple-darwin": Object.freeze({
    tauriBundle: "app",
    portableKind: "macos-app",
    archiveLabel: "macos-x64",
  }),
  "x86_64-pc-windows-msvc": Object.freeze({
    tauriBundle: null,
    portableKind: "windows-directory",
    archiveLabel: "windows-x64",
  }),
});

export function requireReleaseTarget(value) {
  const target = releaseTargets[String(value || "").trim()];
  if (!target) {
    throw new Error(`unsupported release target: ${String(value || "<empty>")}`);
  }
  return target;
}

export function sidecarBuildArguments({ targetTriple, releaseBuild, output, packagePath }) {
  const argumentsList = ["build", "-trimpath"];
  if (releaseBuild && targetTriple === "x86_64-pc-windows-msvc") {
    argumentsList.push("-ldflags", "-H=windowsgui");
  }
  argumentsList.push("-o", output, packagePath);
  return argumentsList;
}

export function parseArguments(argv, allowed) {
  const result = {};
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (!name?.startsWith("--") || value === undefined) {
      throw new Error(`invalid argument near ${name || "<empty>"}`);
    }
    const key = name.slice(2);
    if (!allowed.includes(key) || result[key] !== undefined) {
      throw new Error(`unsupported or duplicate argument: ${name}`);
    }
    result[key] = value;
  }
  return result;
}

export function parseCargoPackage(cargoToml) {
  const packageBlock = cargoToml.match(/^\[package\]\s*([\s\S]*?)(?=^\[|\Z)/m)?.[1];
  if (!packageBlock) {
    throw new Error("Cargo.toml is missing [package]");
  }
  const get = (name) => packageBlock.match(new RegExp(`^${name}\\s*=\\s*"([^"]+)"`, "m"))?.[1];
  return { name: get("name"), version: get("version"), license: get("license") };
}

export async function sha256File(filePath) {
  const content = await readFile(filePath);
  return createHash("sha256").update(content).digest("hex");
}

export function toPosix(value) {
  return value.split(path.sep).join("/");
}
