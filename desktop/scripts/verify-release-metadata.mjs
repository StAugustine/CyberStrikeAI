import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseCargoPackage } from "./release-support.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");

export async function verifyReleaseMetadata(root = repositoryDirectory) {
  const desktop = path.join(root, "desktop");
  const packageJSON = await readJSON(path.join(desktop, "package.json"));
  const packageLock = await readJSON(path.join(desktop, "package-lock.json"));
  const tauriConfig = await readJSON(path.join(desktop, "src-tauri", "tauri.conf.json"));
  const resourceManifest = await readJSON(path.join(desktop, "resources", "manifest.json"));
  const cargoPackage = parseCargoPackage(
    await readFile(path.join(desktop, "src-tauri", "Cargo.toml"), "utf8"),
  );

  const versions = new Map([
    ["package.json", packageJSON.version],
    ["package-lock.json", packageLock.version],
    ["package-lock root", packageLock.packages?.[""]?.version],
    ["Cargo.toml", cargoPackage.version],
    ["tauri.conf.json", tauriConfig.version],
    ["resource manifest", resourceManifest.app_version],
  ]);
  for (const [source, version] of versions) {
    assert(version === packageJSON.version, `${source} version ${version} does not match ${packageJSON.version}`);
  }

  assert(packageJSON.license === "Apache-2.0", "package.json license must be Apache-2.0");
  assert(packageLock.packages?.[""]?.license === "Apache-2.0", "package-lock root license must be Apache-2.0");
  assert(cargoPackage.license === "Apache-2.0", "Cargo.toml license must be Apache-2.0");
  assert(tauriConfig.bundle?.active === true, "Tauri bundling must be active");
  assert(tauriConfig.bundle?.createUpdaterArtifacts === false, "updater artifacts must remain disabled");
  assert(tauriConfig.bundle?.licenseFile === "../../LICENSE", "bundle licenseFile must reference the repository license");
  assert(tauriConfig.bundle?.macOS?.hardenedRuntime === true, "macOS hardened runtime must be enabled");
  assert(tauriConfig.bundle?.macOS?.minimumSystemVersion === "12.0", "minimum supported macOS version must be 12.0");
  assert(
    JSON.stringify(tauriConfig.bundle?.icon) === JSON.stringify([
      "icons/32x32.png",
      "icons/128x128.png",
      "icons/128x128@2x.png",
      "icons/icon.icns",
      "icons/icon.ico",
    ]),
    "desktop icon set must contain the generated Windows and macOS formats",
  );

  const expectedResources = {
    "../resources/manifest.json": "defaults/manifest.json",
    "../../config.example.yaml": "defaults/config.example.yaml",
    "../../agents/": "defaults/agents",
    "../../roles/": "defaults/roles",
    "../../skills/": "defaults/skills",
    "../../tools/": "defaults/tools",
    "../../knowledge_base/": "defaults/knowledge_base",
  };
  assert(
    JSON.stringify(tauriConfig.bundle?.resources) === JSON.stringify(expectedResources),
    "Tauri bundle resources do not match the approved desktop allowlist",
  );
  assert(
    JSON.stringify(tauriConfig.bundle?.externalBin) === JSON.stringify(["binaries/cyberstrike-core"]),
    "Tauri bundle must contain only the fixed desktop core sidecar",
  );
  assert(await exists(path.join(root, "LICENSE")), "repository LICENSE is missing");
  for (const icon of tauriConfig.bundle.icon) {
    assert(await exists(path.join(desktop, "src-tauri", icon)), `desktop icon is missing: ${icon}`);
  }

  return { productName: tauriConfig.productName, identifier: tauriConfig.identifier, version: packageJSON.version };
}

async function readJSON(filePath) {
  return JSON.parse(await readFile(filePath, "utf8"));
}

async function exists(filePath) {
  try {
    await readFile(filePath);
    return true;
  } catch (error) {
    if (error?.code === "ENOENT") return false;
    throw error;
  }
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const metadata = await verifyReleaseMetadata();
  console.log(`verified release metadata: ${metadata.productName} ${metadata.version} (${metadata.identifier})`);
}
