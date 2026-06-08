// mcpServer.ts — the MCP server the Metis CLI connects to. Exposes three
// IDE-backed tools over the Streamable-HTTP transport (stateless mode:
// one transport per request, response streamed back on the POST):
//
//   - getCurrentSelection : the active editor's selected text + location
//   - getDiagnostics      : compiler/linter problems (optionally one file)
//   - openDiff            : show a proposed edit as a diff, apply if accepted
//
// Mirrors the IDE tools Claude Code's extension exposes. Auth is a bearer
// token shared via the lockfile; every request must present it.

import express, { type Request, type Response } from "express";
import * as vscode from "vscode";
import { z } from "zod";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";

function text(s: string) {
  return { content: [{ type: "text" as const, text: s }] };
}

// DIFF_DECISION_TIMEOUT_MS bounds the openDiff modal below the metis MCP
// client's default 60s request timeout, so a slow decision auto-rejects
// (without writing) rather than writing a file the agent already gave up on.
const DIFF_DECISION_TIMEOUT_MS = 50_000;

// buildMcpServer wires the three tools onto a fresh McpServer. A new
// server is built per request (stateless transport), so this stays cheap.
function buildMcpServer(): McpServer {
  const server = new McpServer({ name: "metis-ide", version: "0.1.0" });

  server.tool(
    "getCurrentSelection",
    "Return the text currently selected in the active editor, with its file path and 1-based line/column range. Empty selection returns the cursor position.",
    {},
    async () => {
      const ed = vscode.window.activeTextEditor;
      if (!ed) {
        return text("(no active editor)");
      }
      const sel = ed.selection;
      const selected = ed.document.getText(sel);
      const loc = `${ed.document.uri.fsPath}:${sel.start.line + 1}:${sel.start.character + 1}-${sel.end.line + 1}:${sel.end.character + 1}`;
      if (selected.length === 0) {
        return text(`cursor at ${loc} (no selection)`);
      }
      return text(`selection at ${loc}\n---\n${selected}`);
    },
  );

  server.tool(
    "getDiagnostics",
    "Return compiler/linter diagnostics (errors, warnings, hints). Pass `uri` (a file path) to scope to one file; omit to return diagnostics for all files that have any.",
    { uri: z.string().optional().describe("absolute file path to scope to") },
    async ({ uri }) => {
      const sev = ["error", "warning", "info", "hint"];
      const lines: string[] = [];
      const render = (u: vscode.Uri, ds: readonly vscode.Diagnostic[]) => {
        for (const d of ds) {
          const r = d.range.start;
          lines.push(`${u.fsPath}:${r.line + 1}:${r.character + 1}: ${sev[d.severity] ?? "?"}: ${d.message}`);
        }
      };
      if (uri) {
        render(vscode.Uri.file(uri), vscode.languages.getDiagnostics(vscode.Uri.file(uri)));
      } else {
        for (const [u, ds] of vscode.languages.getDiagnostics()) {
          if (ds.length > 0) {
            render(u, ds);
          }
        }
      }
      return text(lines.length === 0 ? "(no diagnostics)" : lines.join("\n"));
    },
  );

  server.tool(
    "openDiff",
    "Show a proposed edit to a file as a side-by-side diff and ask the user to Accept or Reject it. On Accept the file is written. Returns 'accepted' or 'rejected'. Use this instead of writing files directly when an IDE is attached so the user can review changes inline.",
    {
      path: z.string().describe("absolute path of the file to edit"),
      newText: z.string().describe("the full proposed new contents of the file"),
      description: z.string().optional().describe("short summary of the change, shown to the user"),
    },
    async ({ path: filePath, newText, description }) => {
      const fileUri = vscode.Uri.file(filePath);
      // Virtual document holding the proposed content for the right pane.
      const proposed = vscode.Uri.parse(`metis-diff:${filePath}?${Date.now()}`);
      const key = proposed.toString();
      proposedContents.set(key, newText);
      try {
        const title = description ? `Metis: ${description}` : `Metis proposed edit — ${filePath}`;
        await vscode.commands.executeCommand("vscode.diff", fileUri, proposed, title);

        // The metis CLI cancels this RPC after MCP_REQUEST_TIMEOUT (60s
        // default). If we let the modal block past that, the agent has
        // already given up — but a late Accept would still WRITE the file,
        // clobbering whatever happened since. So bound the wait below the
        // client timeout and auto-reject (no write) if it elapses.
        const pick = await Promise.race([
          vscode.window.showInformationMessage(
            description ? `Apply Metis edit? (${description})` : "Apply Metis proposed edit?",
            { modal: true },
            "Accept",
            "Reject",
          ),
          new Promise<undefined>((r) => setTimeout(() => r(undefined), DIFF_DECISION_TIMEOUT_MS)),
        ]);
        if (pick !== "Accept") {
          return text(
            pick === "Reject" ? "rejected" : "rejected (no response in time — raise MCP_REQUEST_TIMEOUT for longer reviews)",
          );
        }
        await vscode.workspace.fs.writeFile(fileUri, new TextEncoder().encode(newText));
        return text("accepted");
      } finally {
        // Always release the virtual-doc backing, even if diff/modal threw.
        proposedContents.delete(key);
      }
    },
  );

  return server;
}

// proposedContents backs the metis-diff:// virtual documents shown in the
// right pane of openDiff. Registered once by registerDiffProvider.
const proposedContents = new Map<string, string>();

export function registerDiffProvider(ctx: vscode.ExtensionContext): void {
  const provider: vscode.TextDocumentContentProvider = {
    provideTextDocumentContent(uri) {
      return proposedContents.get(uri.toString()) ?? "";
    },
  };
  ctx.subscriptions.push(
    vscode.workspace.registerTextDocumentContentProvider("metis-diff", provider),
  );
}

// createApp returns an express app serving the Streamable-HTTP MCP
// endpoint at POST /mcp, gated by the bearer token.
export function createApp(token: string): express.Express {
  const app = express();
  app.use(express.json({ limit: "16mb" }));

  app.post("/mcp", async (req: Request, res: Response) => {
    if (req.header("authorization") !== `Bearer ${token}`) {
      res.status(401).json({ error: "unauthorized" });
      return;
    }
    // Stateless: a fresh server + transport per request, torn down when
    // the response closes.
    const server = buildMcpServer();
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    res.on("close", () => {
      transport.close();
      server.close();
    });
    try {
      await server.connect(transport);
      await transport.handleRequest(req, res, req.body);
    } catch (err) {
      if (!res.headersSent) {
        res.status(500).json({ error: String(err) });
      }
    }
  });

  return app;
}
