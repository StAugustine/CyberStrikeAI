import { createHash } from "node:crypto";
import { lstat, mkdir, readFile, readdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const desktopDir = path.resolve(scriptDir, "..");
const workspaceDir = path.resolve(desktopDir, "..");
const outputPath = path.join(desktopDir, "resources", "manifest.json");
const packageJSON = JSON.parse(await readFile(path.join(desktopDir, "package.json"), "utf8"));

const inputs = [
  "config.example.yaml",
  "agents",
  "roles",
  "skills",
  "tools",
  "knowledge_base",
];

const files = [];
for (const input of inputs) {
  await collect(path.join(workspaceDir, input), input.replaceAll(path.sep, "/"));
}
files.sort((left, right) => (left.path < right.path ? -1 : left.path > right.path ? 1 : 0));

const manifest = {
  schema_version: 1,
  app_version: packageJSON.version,
  files,
};

await mkdir(path.dirname(outputPath), { recursive: true });
await writeFile(outputPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");

async function collect(absolutePath, manifestPath) {
  const info = await lstat(absolutePath);
  if (info.isSymbolicLink()) {
    throw new Error(`resource input must not be a symbolic link: ${manifestPath}`);
  }
  if (info.isFile()) {
    const content = await readFile(absolutePath);
    files.push({
      path: manifestPath,
      sha256: createHash("sha256").update(content).digest("hex"),
    });
    return;
  }
  if (!info.isDirectory()) {
    throw new Error(`unsupported resource input: ${manifestPath}`);
  }
  const entries = await readdir(absolutePath);
  entries.sort((left, right) => (left < right ? -1 : left > right ? 1 : 0));
  for (const entry of entries) {
    await collect(path.join(absolutePath, entry), `${manifestPath}/${entry}`);
  }
}
