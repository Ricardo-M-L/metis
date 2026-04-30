#!/usr/bin/env node
// Downloads the right native binary for this platform.
// LOCAL-ONLY: fetches from METIS_RELEASE_BASE (default: file://$HOME/.metis-releases).
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const crypto = require("node:crypto");
const https = require("node:https");
const { URL } = require("node:url");
const { execFileSync } = require("node:child_process");
const os = require("node:os");
const zlib = require("node:zlib");

const PKG_VERSION = require("../package.json").version;
const RELEASE_BASE = (process.env.METIS_RELEASE_BASE || `file://${os.homedir()}/.metis-releases`).replace(/\/+$/, "");
const VERSION = process.env.METIS_VERSION || PKG_VERSION;

function detectTarget() {
  const platform = process.platform;
  const arch = process.arch;
  const osMap = { darwin: "darwin", linux: "linux" };
  const archMap = { arm64: "arm64", x64: "amd64" };
  if (!osMap[platform] || !archMap[arch]) {
    throw new Error(`unsupported platform: ${platform}-${arch}`);
  }
  return `${osMap[platform]}-${archMap[arch]}`;
}

function fetchBuf(url) {
  return new Promise((resolve, reject) => {
    if (url.startsWith("file://")) {
      try { resolve(fs.readFileSync(url.slice(7))); } catch (e) { reject(e); }
      return;
    }
    https.get(url, (res) => {
      if (res.statusCode === 302 || res.statusCode === 301) {
        return fetchBuf(new URL(res.headers.location, url).toString()).then(resolve, reject);
      }
      if (res.statusCode !== 200) {
        return reject(new Error(`HTTP ${res.statusCode} for ${url}`));
      }
      const chunks = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => resolve(Buffer.concat(chunks)));
      res.on("error", reject);
    }).on("error", reject);
  });
}

async function main() {
  const target = detectTarget();
  const artifact = `metis-${target}.tar.gz`;
  const base = `${RELEASE_BASE}/${VERSION}`;
  console.log(`metis: downloading ${artifact} from ${base}`);

  const tarBuf = await fetchBuf(`${base}/${artifact}`);
  const sumBuf = await fetchBuf(`${base}/${artifact}.sha256`);

  const expected = sumBuf.toString().trim().split(/\s+/)[0];
  const actual = crypto.createHash("sha256").update(tarBuf).digest("hex");
  if (expected !== actual) {
    throw new Error(`sha256 mismatch:\n  expected ${expected}\n  actual   ${actual}`);
  }

  const vendor = path.join(__dirname, "..", "vendor");
  fs.mkdirSync(vendor, { recursive: true });
  const tarPath = path.join(vendor, artifact);
  fs.writeFileSync(tarPath, tarBuf);

  // Extract using system tar (avoids extra deps)
  execFileSync("tar", ["-xzf", tarPath, "-C", vendor], { stdio: "inherit" });
  fs.unlinkSync(tarPath);

  const unpackedName = `metis-${target}`;
  const finalName = process.platform === "win32" ? "metis.exe" : "metis";
  const src = path.join(vendor, unpackedName);
  const dst = path.join(vendor, finalName);
  if (src !== dst) fs.renameSync(src, dst);
  fs.chmodSync(dst, 0o755);

  console.log(`metis: installed ${dst}`);
}

main().catch((err) => {
  console.error(`metis postinstall failed: ${err.message}`);
  console.error(`(set METIS_RELEASE_BASE to override; current: ${RELEASE_BASE})`);
  process.exit(1);
});
