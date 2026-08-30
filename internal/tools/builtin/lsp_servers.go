package builtin

// lsp_servers.go — per-language LSP server registry. Maps a detected
// language to the binary metis spawns and the JSON-RPC handshake it
// speaks (via lsp_client.go). Go also uses stdio: didOpen lets Metis send the
// bytes from the exact descriptor authorized for this invocation instead of
// asking the gopls CLI to reopen a raceable pathname.

import "os/exec"

// lspServer describes how to launch and root one language server.
type lspServer struct {
	lang string // matches detectLanguage output
	// cmd + args spawn the server speaking LSP on stdin/stdout. Most
	// servers need an explicit "--stdio" flag; rust-analyzer defaults to
	// stdio with no args.
	cmd  string
	args []string
	// languageID is the LSP `languageId` sent in textDocument/didOpen.
	// Usually equals lang but kept separate (e.g. "typescriptreact").
	languageID string
	// rootMarkers are filenames whose nearest ancestor dir is the project
	// root (rootUri). Falls back to the file's own dir if none found.
	rootMarkers []string
}

// stdioLSPServers is the language-server table. Order doesn't matter — lookup
// is by language.
var stdioLSPServers = []lspServer{
	{
		lang:        "go",
		cmd:         "gopls",
		languageID:  "go",
		rootMarkers: []string{"go.work", "go.mod", ".git"},
	},
	{
		lang:        "python",
		cmd:         "pyright-langserver",
		args:        []string{"--stdio"},
		languageID:  "python",
		rootMarkers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", ".git"},
	},
	{
		lang:        "typescript",
		cmd:         "typescript-language-server",
		args:        []string{"--stdio"},
		languageID:  "typescript",
		rootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json", ".git"},
	},
	{
		lang:        "javascript",
		cmd:         "typescript-language-server",
		args:        []string{"--stdio"},
		languageID:  "javascript",
		rootMarkers: []string{"jsconfig.json", "tsconfig.json", "package.json", ".git"},
	},
	{
		lang:        "rust",
		cmd:         "rust-analyzer",
		args:        nil,
		languageID:  "rust",
		rootMarkers: []string{"Cargo.toml", ".git"},
	},
}

// stdioLSPServerFor returns the server config for a language, if metis
// knows one.
func stdioLSPServerFor(lang string) (lspServer, bool) {
	for _, s := range stdioLSPServers {
		if s.lang == lang {
			return s, true
		}
	}
	return lspServer{}, false
}

// available reports whether this server's binary is on PATH.
func (s lspServer) available() bool {
	_, err := exec.LookPath(s.cmd)
	return err == nil
}

// anyLSPServerAvailable reports whether at least one LSP backend metis
// supports is installed. Used
// by LSP.IsEnabled so the tool is hidden only when NO backend exists.
func anyLSPServerAvailable() bool {
	for _, s := range stdioLSPServers {
		if s.available() {
			return true
		}
	}
	return false
}
