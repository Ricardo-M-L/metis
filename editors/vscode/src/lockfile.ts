// lockfile.ts — writes the discovery lockfile the Metis CLI scans.
//
// Path:   ~/.metis/ide/<port>.lock
// Format: must match internal/runtime/ide.go's IDELock struct exactly.
//
//   { pid, port, transport, ideName, workspaceFolders, authToken }
//
// Metis reads this on startup, verifies the pid is alive, matches
// workspaceFolders against its cwd, and connects to
// http://127.0.0.1:<port>/mcp with `Authorization: Bearer <authToken>`.

import * as fs from "fs";
import * as os from "os";
import * as path from "path";

export interface IDELock {
  pid: number;
  port: number;
  transport: "http";
  ideName: string;
  workspaceFolders: string[];
  authToken: string;
}

function metisHome(): string {
  const override = process.env.METIS_HOME;
  if (override && override.length > 0) {
    return override;
  }
  return path.join(os.homedir(), ".metis");
}

export function ideDir(): string {
  return path.join(metisHome(), "ide");
}

export function lockPath(port: number): string {
  return path.join(ideDir(), `${port}.lock`);
}

export function writeLock(lock: IDELock): string {
  const dir = ideDir();
  fs.mkdirSync(dir, { recursive: true });
  const p = lockPath(lock.port);
  // 0600: the auth token is a bearer secret — don't leave it world-readable.
  fs.writeFileSync(p, JSON.stringify(lock, null, 2), { mode: 0o600 });
  return p;
}

export function removeLock(port: number): void {
  try {
    fs.unlinkSync(lockPath(port));
  } catch {
    // already gone — fine
  }
}
