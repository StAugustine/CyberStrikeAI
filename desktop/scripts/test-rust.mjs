import { execFileSync, spawnSync } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { tauriBuildConfigForTarget } from "./build-config.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const targetTriple = process.env.CYBERSTRIKE_DESKTOP_TARGET
  || execFileSync("rustc", ["--print", "host-tuple"], { encoding: "utf8" }).trim();
const result = spawnSync(
  "cargo",
  ["test", "--locked", "--manifest-path", path.join("src-tauri", "Cargo.toml")],
  {
    cwd: desktopDirectory,
    env: {
      ...process.env,
      CYBERSTRIKE_DESKTOP_TARGET: targetTriple,
      TAURI_CONFIG: JSON.stringify(tauriBuildConfigForTarget(targetTriple)),
    },
    stdio: "inherit",
  },
);
if (result.error) throw result.error;
if (result.status !== 0) process.exit(result.status ?? 1);
