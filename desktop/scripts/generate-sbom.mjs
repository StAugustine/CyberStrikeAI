import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { parseArguments } from "./release-support.mjs";

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const desktopDirectory = path.resolve(scriptDirectory, "..");
const repositoryDirectory = path.resolve(desktopDirectory, "..");

export async function createSBOM(root = repositoryDirectory) {
  const desktop = path.join(root, "desktop");
  const packageJSON = JSON.parse(await readFile(path.join(desktop, "package.json"), "utf8"));
  const packageLock = JSON.parse(await readFile(path.join(desktop, "package-lock.json"), "utf8"));
  const cargoLock = await readFile(path.join(desktop, "src-tauri", "Cargo.lock"), "utf8");
  const goMod = await readFile(path.join(root, "go.mod"), "utf8");

  const components = [
    ...parseGoComponents(goMod),
    ...parseCargoComponents(cargoLock, packageJSON),
    ...parseNPMComponents(packageLock),
  ];
  const unique = new Map();
  for (const component of components) unique.set(component["bom-ref"], component);
  const sorted = [...unique.values()].sort((left, right) => left["bom-ref"].localeCompare(right["bom-ref"]));
  const applicationRef = `pkg:generic/cyberstrikeai-desktop@${packageJSON.version}`;

  return {
    bomFormat: "CycloneDX",
    specVersion: "1.6",
    version: 1,
    metadata: {
      component: {
        type: "application",
        name: "CyberStrikeAI Desktop",
        version: packageJSON.version,
        "bom-ref": applicationRef,
        licenses: [{ license: { id: "Apache-2.0" } }],
      },
    },
    components: sorted,
    dependencies: [
      { ref: applicationRef, dependsOn: sorted.map((component) => component["bom-ref"]) },
      ...sorted.map((component) => ({ ref: component["bom-ref"], dependsOn: [] })),
    ],
  };
}

export function parseGoComponents(goMod) {
  const replacements = new Map();
  for (const match of goMod.matchAll(/^replace\s+(\S+)(?:\s+\S+)?\s+=>\s+(\S+)\s+(v\S+)\s*$/gm)) {
    replacements.set(match[1], { name: match[2], version: match[3] });
  }
  const dependencies = [];
  for (const block of goMod.matchAll(/^require\s*\(([\s\S]*?)^\)/gm)) {
    for (const line of block[1].split(/\r?\n/)) {
      const match = line.match(/^\s*(\S+)\s+(v\S+?)(\s+\/\/\s+indirect)?\s*$/);
      if (!match) continue;
      const replacement = replacements.get(match[1]);
      dependencies.push({
        name: replacement?.name || match[1],
        version: replacement?.version || match[2],
        indirect: Boolean(match[3]),
        replaced: replacement ? match[1] : undefined,
      });
    }
  }
  return dependencies.map((dependency) => {
    const purl = `pkg:golang/${encodePath(dependency.name)}@${encodeURIComponent(dependency.version)}`;
    const properties = [
      property("cyberstrikeai:ecosystem", "go"),
      property("cyberstrikeai:dependency-scope", dependency.indirect ? "indirect" : "direct"),
    ];
    if (dependency.replaced) properties.push(property("cyberstrikeai:replaces", dependency.replaced));
    return library(dependency.name, dependency.version, purl, properties);
  });
}

export function parseCargoComponents(cargoLock, application) {
  return cargoLock
    .split(/^\[\[package\]\]\s*$/m)
    .slice(1)
    .map((block) => ({
      name: field(block, "name"),
      version: field(block, "version"),
      source: field(block, "source"),
      checksum: field(block, "checksum"),
    }))
    .filter((item) => item.name && item.version)
    .filter((item) => item.name !== application.name || item.version !== application.version)
    .map((item) => {
      const purl = `pkg:cargo/${encodeURIComponent(item.name)}@${encodeURIComponent(item.version)}`;
      const component = library(item.name, item.version, purl, [property("cyberstrikeai:ecosystem", "cargo")]);
      if (/^[a-f0-9]{64}$/i.test(item.checksum || "")) {
        component.hashes = [{ alg: "SHA-256", content: item.checksum.toLowerCase() }];
      }
      if (item.source) component.properties.push(property("cyberstrikeai:source", item.source));
      return component;
    });
}

export function parseNPMComponents(packageLock) {
  const components = [];
  for (const [packagePath, item] of Object.entries(packageLock.packages || {})) {
    if (!packagePath || !item.version) continue;
    const name = item.name || packagePath.split("node_modules/").at(-1);
    if (!name) continue;
    const purl = `pkg:npm/${encodeNPMName(name)}@${encodeURIComponent(item.version)}`;
    const properties = [property("cyberstrikeai:ecosystem", "npm")];
    if (item.dev) properties.push(property("cyberstrikeai:dependency-scope", "development"));
    if (item.optional) properties.push(property("cyberstrikeai:optional", "true"));
    const component = library(name, item.version, purl, properties);
    const integrity = item.integrity?.match(/^sha512-(.+)$/);
    if (integrity) {
      component.hashes = [{ alg: "SHA-512", content: Buffer.from(integrity[1], "base64").toString("hex") }];
    }
    if (item.license) component.licenses = [{ expression: item.license }];
    components.push(component);
  }
  return components;
}

function library(name, version, purl, properties) {
  return { type: "library", name, version, "bom-ref": purl, purl, properties };
}

function property(name, value) {
  return { name, value };
}

function field(block, name) {
  return block.match(new RegExp(`^${name}\\s*=\\s*"([^"]+)"`, "m"))?.[1];
}

function encodePath(value) {
  return value.split("/").map(encodeURIComponent).join("/");
}

function encodeNPMName(value) {
  if (!value.startsWith("@")) return encodeURIComponent(value);
  const [scope, ...rest] = value.split("/");
  return `${encodeURIComponent(scope)}/${rest.map(encodeURIComponent).join("/")}`;
}

export function stableSBOMDigest(sbom) {
  return createHash("sha256").update(`${JSON.stringify(sbom, null, 2)}\n`).digest("hex");
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const args = parseArguments(process.argv.slice(2), ["output"]);
  const output = path.resolve(args.output || process.env.CYBERSTRIKE_RELEASE_SBOM || "");
  if (!args.output && !process.env.CYBERSTRIKE_RELEASE_SBOM) {
    throw new Error("--output or CYBERSTRIKE_RELEASE_SBOM is required");
  }
  const sbom = await createSBOM();
  await mkdir(path.dirname(output), { recursive: true });
  await writeFile(output, `${JSON.stringify(sbom, null, 2)}\n`, "utf8");
  console.log(`wrote deterministic CycloneDX SBOM (${sbom.components.length} components): ${output}`);
}
