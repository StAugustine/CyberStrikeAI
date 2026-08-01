import { execFileSync, spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { requireReleaseTarget } from "./release-support.mjs";
import { verifyReleaseMetadata } from "./verify-release-metadata.mjs";
import { writeTauriBuildConfig } from "./build-config.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const requestedTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync("rustc", ["--print", "host-tuple"], { encoding: "utf8" }).trim();
const releaseTarget = requireReleaseTarget(requestedTriple);

await verifyReleaseMetadata();
run(process.execPath, [path.join(scriptDirectory, "generate-resource-manifest.mjs")]);
await verifyReleaseMetadata();
run(process.execPath, [path.join(scriptDirectory, "build-sidecar.mjs")]);
const generatedTauriConfig = writeTauriBuildConfig(requestedTriple);
const tauriArguments = [
  path.join(desktopDirectory, "node_modules", "@tauri-apps", "cli", "tauri.js"),
  "build",
  "--target",
  requestedTriple,
  "--ci",
  "--no-sign",
  "--config",
  generatedTauriConfig,
];
if (releaseTarget.tauriBundle) {
  tauriArguments.push("--bundles", releaseTarget.tauriBundle);
} else {
  tauriArguments.push("--no-bundle");
}
run(process.execPath, tauriArguments);

function run(command, args) {
  const result = spawnSync(command, args, {
    cwd: desktopDirectory,
    env: { ...process.env, CYBERSTRIKE_DESKTOP_TARGET: requestedTriple },
    stdio: "inherit",
  });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}
