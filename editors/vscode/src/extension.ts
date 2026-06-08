// extension.ts — Metis IDE bridge lifecycle.
//
// On activation: start the MCP server (mcpServer.ts) on a free port, write
// a discovery lockfile (lockfile.ts) so the Metis CLI can find and
// authenticate to it. On deactivation: remove the lockfile and stop the
// server. The CLI, when launched in this workspace, attaches to the
// server and gains the getCurrentSelection / getDiagnostics / openDiff
// tools.

import * as crypto from "crypto";
import * as http from "http";
import * as vscode from "vscode";
import { createApp, registerDiffProvider } from "./mcpServer";
import { removeLock, writeLock } from "./lockfile";

let server: http.Server | undefined;
let activePort = 0;

function workspaceFolders(): string[] {
  return (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
}

async function start(ctx: vscode.ExtensionContext): Promise<void> {
  const cfg = vscode.workspace.getConfiguration("metis.ide");
  if (!cfg.get<boolean>("enabled", true)) {
    return;
  }
  const token = crypto.randomBytes(24).toString("hex");
  const app = createApp(token);
  const desiredPort = cfg.get<number>("port", 0) ?? 0;

  await new Promise<void>((resolve, reject) => {
    // Bind to loopback only — never expose the bridge off-host.
    server = http.createServer(app).listen(desiredPort, "127.0.0.1", () => {
      const addr = server!.address();
      activePort = typeof addr === "object" && addr ? addr.port : desiredPort;
      try {
        writeLock({
          pid: process.pid,
          port: activePort,
          transport: "http",
          ideName: vscode.env.appName || "VS Code",
          workspaceFolders: workspaceFolders(),
          authToken: token,
        });
      } catch (err) {
        // Lockfile write failed (e.g. ~/.metis unwritable). The server is
        // already listening — close it so we don't strand an orphan
        // listener holding the port with no discoverable lockfile.
        server?.close();
        server = undefined;
        activePort = 0;
        reject(err);
        return;
      }
      resolve();
    });
    server!.on("error", reject);
  });

  ctx.subscriptions.push({ dispose: stop });
  vscode.window.setStatusBarMessage(`Metis IDE bridge on :${activePort}`, 4000);
}

function stop(): void {
  if (activePort) {
    removeLock(activePort);
  }
  server?.close();
  server = undefined;
  activePort = 0;
}

export function activate(ctx: vscode.ExtensionContext): void {
  registerDiffProvider(ctx);

  ctx.subscriptions.push(
    vscode.commands.registerCommand("metis.ide.status", () => {
      vscode.window.showInformationMessage(
        activePort
          ? `Metis IDE bridge running on http://127.0.0.1:${activePort}/mcp`
          : "Metis IDE bridge is not running.",
      );
    }),
    vscode.commands.registerCommand("metis.ide.restart", async () => {
      stop();
      await start(ctx).catch((e) => vscode.window.showErrorMessage(`Metis IDE: ${e}`));
    }),
  );

  start(ctx).catch((e) => vscode.window.showErrorMessage(`Metis IDE: failed to start bridge: ${e}`));
}

export function deactivate(): void {
  stop();
}
