import { execFileSync, spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { writeTauriBuildConfig } from "./build-config.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync("rustc", ["--print", "host-tuple"], { encoding: "utf8" }).trim();
const environment = { ...process.env, CYBERSTRIKE_DESKTOP_TARGET: targetTriple };

run(process.execPath, [path.join(scriptDirectory, "build-sidecar.mjs")]);
const tauriConfig = writeTauriBuildConfig(targetTriple);
run(process.execPath, [
  path.join(desktopDirectory, "node_modules", "@tauri-apps", "cli", "tauri.js"),
  "build",
  "--debug",
  "--no-bundle",
  "--config",
  tauriConfig,
]);

function run(command, args) {
  const result = spawnSync(command, args, {
    cwd: desktopDirectory,
    env: environment,
    stdio: "inherit",
  });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}
