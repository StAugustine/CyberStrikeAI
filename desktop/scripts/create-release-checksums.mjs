import { lstat, readFile, readdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments, requireReleaseTarget, sha256File } from "./release-support.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");

export async function createReleaseChecksums({ targetTriple, directory, root = desktopDirectory }) {
  requireReleaseTarget(targetTriple);
  const packageJSON = JSON.parse(await readFile(path.join(root, "package.json"), "utf8"));
  const entries = await readdir(directory, { withFileTypes: true });
  const names = entries
    .filter((entry) => entry.isFile() && !["SHA256SUMS", "release-manifest.json"].includes(entry.name))
    .map((entry) => entry.name)
    .sort((left, right) => left.localeCompare(right));
  const archives = names.filter((name) => name.toLowerCase().endsWith("-portable.zip"));
  if (archives.length !== 1) throw new Error(`staged release must contain one portable ZIP, found ${archives.length}`);
  for (const required of ["audit-report.json", "portable-runtime-report.json", "sbom.cdx.json"]) {
    if (!names.includes(required)) throw new Error(`staged release is missing ${required}`);
  }

  const files = [];
  for (const name of names) {
    const filePath = path.join(directory, name);
    const info = await lstat(filePath);
    files.push({ name, size: info.size, sha256: await sha256File(filePath) });
  }
  const manifest = {
    schemaVersion: 1,
    product: "CyberStrikeAI Desktop",
    version: packageJSON.version,
    target: targetTriple,
    format: "portable-zip",
    portable: true,
    unsigned: true,
    files,
  };
  const sums = files.map((file) => `${file.sha256}  ${file.name}`).join("\n");
  await writeFile(path.join(directory, "SHA256SUMS"), `${sums}\n`, "utf8");
  await writeFile(path.join(directory, "release-manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  return manifest;
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const args = parseArguments(process.argv.slice(2), ["target", "directory"]);
  const targetTriple = args.target || process.env.CYBERSTRIKE_DESKTOP_TARGET;
  const directory = path.resolve(args.directory || process.env.CYBERSTRIKE_RELEASE_OUTPUT || "");
  if (!targetTriple || (!args.directory && !process.env.CYBERSTRIKE_RELEASE_OUTPUT)) {
    throw new Error("target and staged release directory are required");
  }
  const manifest = await createReleaseChecksums({ targetTriple, directory });
  console.log(`wrote release manifest and checksums for ${manifest.files.length} files`);
}
