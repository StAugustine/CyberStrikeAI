import { lstat, readFile, readdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments, requireReleaseTarget, sha256File, toPosix } from "./release-support.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");

const excludedWebAssets = [
  "static/css/c2.css",
  "static/js/c2.js",
  "static/js/terminal.js",
  "static/js/webshell.js",
  "static/js/wechat-robot.js",
  "static/js/rbac.js",
  "static/vendor/xterm.js",
  "static/vendor/xterm-addon-fit.js",
  "static/vendor/xterm.css",
];
const approvedResourceRoots = ["agents/", "roles/", "skills/", "tools/", "knowledge_base/"];

export async function auditRelease({ root = repositoryDirectory, targetTriple, stageDirectory, outputDirectory }) {
  const releaseTarget = requireReleaseTarget(targetTriple);
  const portableRoot = await onlyEntry(stageDirectory, (entry) => entry.isDirectory(), "portable root directory");
  const archivePath = await onlyEntry(
    outputDirectory,
    (entry) => entry.isFile() && entry.name.toLowerCase().endsWith(".zip"),
    "portable ZIP archive",
  );

  const inventory = [];
  await collectInventory(portableRoot, portableRoot, inventory);
  inventory.sort((left, right) => left.path.localeCompare(right.path));
  validatePortableLayout(releaseTarget.portableKind, inventory);

  const resourceAudit = await auditResources(root);
  const embedAudit = await auditDesktopEmbedAllowlist(root);
  const packageJSON = JSON.parse(await readFile(path.join(root, "desktop", "package.json"), "utf8"));
  const archiveInfo = await lstat(archivePath);
  const report = {
    schemaVersion: 1,
    product: "CyberStrikeAI Desktop",
    version: packageJSON.version,
    target: targetTriple,
    format: "portable-zip",
    result: "passed",
    checks: {
      portableLayout: releaseTarget.portableKind,
      portableInventoryEntries: inventory.length,
      approvedDefaultResources: resourceAudit.count,
      defaultResourceManifestSHA256: resourceAudit.manifestSHA256,
      excludedWebAssets: embedAudit,
      sensitiveFilenamePatterns: "passed",
      privateKeyAndAccessKeyPatterns: "passed",
    },
    archive: {
      name: path.basename(archivePath),
      size: archiveInfo.size,
      sha256: await sha256File(archivePath),
    },
    portableRoot: path.basename(portableRoot),
    inventory,
  };
  await writeFile(path.join(outputDirectory, "audit-report.json"), `${JSON.stringify(report, null, 2)}\n`, "utf8");
  return report;
}

function validatePortableLayout(kind, inventory) {
  const filePaths = new Set(inventory.filter((item) => item.type === "file").map((item) => item.path));
  for (const required of ["LICENSE", "README.txt"]) {
    if (!filePaths.has(required)) throw new Error(`portable package is missing ${required}`);
  }
  if (kind === "windows-directory") {
    for (const required of [
      "CyberStrikeAI Desktop.exe",
      "cyberstrike-core.exe",
      "cyberstrike-native-host.exe",
      "defaults/manifest.json",
      "defaults/config.example.yaml",
    ]) {
      if (!filePaths.has(required)) throw new Error(`Windows portable package is missing ${required}`);
    }
    return;
  }
  const requiredSuffixes = [
    ".app/Contents/Info.plist",
    ".app/Contents/MacOS/cyberstrike-desktop",
    ".app/Contents/MacOS/cyberstrike-core",
    ".app/Contents/MacOS/cyberstrike-native-host",
    ".app/Contents/Resources/defaults/manifest.json",
    ".app/Contents/Resources/defaults/config.example.yaml",
  ];
  for (const suffix of requiredSuffixes) {
    if (![...filePaths].some((filePath) => filePath.endsWith(suffix))) {
      throw new Error(`macOS portable package is missing an entry ending in ${suffix}`);
    }
  }
}

export function validateReleasePath(resourcePath) {
  const normalized = resourcePath.replaceAll("\\", "/");
  const basename = normalized.split("/").at(-1) || "";
  const approvedExample = normalized === "config.example.yaml" || normalized.endsWith("/config.example.yaml");
  if (!approvedExample && (/^\.env(?:\.|$)/i.test(basename) || /^config\.ya?ml$/i.test(basename))) {
    throw new Error(`sensitive configuration filename is not allowed: ${resourcePath}`);
  }
  if (/\.(?:db|sqlite3?|pem|key|p12|pfx|log|tmp)$/i.test(basename) || basename === ".DS_Store" || basename.endsWith("~")) {
    throw new Error(`sensitive or temporary filename is not allowed: ${resourcePath}`);
  }
}

export function validateSensitiveContent(content, resourcePath) {
  const text = Buffer.isBuffer(content) ? content.toString("utf8") : String(content);
  if (/-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----/.test(text)) {
    throw new Error(`private key material is not allowed: ${resourcePath}`);
  }
  if (/(?:^|[^A-Z0-9])AKIA[A-Z0-9]{16}(?:[^A-Z0-9]|$)/.test(text)) {
    throw new Error(`AWS access key material is not allowed: ${resourcePath}`);
  }
}

async function auditResources(root) {
  const manifestPath = path.join(root, "desktop", "resources", "manifest.json");
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  for (const item of manifest.files || []) {
    if (item.path !== "config.example.yaml" && !approvedResourceRoots.some((prefix) => item.path.startsWith(prefix))) {
      throw new Error(`resource is outside the approved desktop roots: ${item.path}`);
    }
    validateReleasePath(item.path);
    const sourcePath = path.join(root, ...item.path.split("/"));
    const content = await readFile(sourcePath);
    validateSensitiveContent(content, item.path);
    const actualHash = await sha256File(sourcePath);
    if (actualHash !== item.sha256) throw new Error(`resource hash does not match manifest: ${item.path}`);
  }
  return { count: manifest.files?.length || 0, manifestSHA256: await sha256File(manifestPath) };
}

async function auditDesktopEmbedAllowlist(root) {
  const assetsSource = await readFile(path.join(root, "web", "assets.go"), "utf8");
  const directives = assetsSource.split(/\r?\n/).filter((line) => line.startsWith("//go:embed ")).join("\n");
  for (const excluded of excludedWebAssets) {
    if (directives.split(/\s+/).includes(excluded)) throw new Error(`excluded desktop asset is embedded: ${excluded}`);
  }
  return excludedWebAssets.map((asset) => ({ asset, result: "excluded" }));
}

async function onlyEntry(directory, predicate, label) {
  const entries = (await readdir(directory, { withFileTypes: true })).filter(predicate);
  if (entries.length !== 1) throw new Error(`expected one ${label}, found ${entries.length}`);
  return path.join(directory, entries[0].name);
}

async function collectInventory(root, current, inventory) {
  const entries = await readdir(current, { withFileTypes: true });
  entries.sort((left, right) => left.name.localeCompare(right.name));
  for (const entry of entries) {
    const absolute = path.join(current, entry.name);
    const relative = toPosix(path.relative(root, absolute));
    validateReleasePath(relative);
    if (entry.isSymbolicLink()) {
      inventory.push({ path: relative, type: "symlink" });
    } else if (entry.isDirectory()) {
      inventory.push({ path: relative, type: "directory" });
      await collectInventory(root, absolute, inventory);
    } else if (entry.isFile()) {
      const info = await lstat(absolute);
      inventory.push({ path: relative, type: "file", size: info.size, sha256: await sha256File(absolute) });
    } else {
      throw new Error(`unsupported bundle entry: ${relative}`);
    }
  }
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const args = parseArguments(process.argv.slice(2), ["target", "stage-directory", "output-directory"]);
  const targetTriple = args.target || process.env.CYBERSTRIKE_DESKTOP_TARGET;
  const stageDirectory = path.resolve(args["stage-directory"] || process.env.CYBERSTRIKE_PORTABLE_STAGE || "");
  const outputDirectory = path.resolve(args["output-directory"] || process.env.CYBERSTRIKE_RELEASE_OUTPUT || "");
  if (!targetTriple || (!args["stage-directory"] && !process.env.CYBERSTRIKE_PORTABLE_STAGE)
    || (!args["output-directory"] && !process.env.CYBERSTRIKE_RELEASE_OUTPUT)) {
    throw new Error("target, portable stage directory and output directory are required");
  }
  const report = await auditRelease({ targetTriple, stageDirectory, outputDirectory });
  console.log(`portable release audit passed: ${report.target}, ${report.archive.name}`);
}
