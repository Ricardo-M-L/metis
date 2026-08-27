package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/desktop"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/webui"
)

var launchNativeDesktop = desktop.LaunchApp

// cmdDesktop implements `metis desktop`. The native Wails client is the
// default; the old browser UI remains available behind --web for development
// and backwards compatibility.
//
// Usage:
//
//	metis desktop                   # native desktop app
//	metis desktop --web             # browser UI on port 8080
//	metis desktop --web --port 9090 # browser UI on a custom port
func cmdDesktop(ctx context.Context, args []string) error {
	opts, err := parseDesktopOptions(args, os.Getenv)
	if err != nil {
		return err
	}
	if opts.help {
		fmt.Print(desktopHelp)
		return nil
	}
	if !opts.web {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("desktop: determine workspace: %w", err)
		}
		return launchNativeDesktop(cwd)
	}

	flags := &cliFlags{autoMemoryStartup: autoMemoryStartupDesktop}
	presetName := "standard"
	if prefs, prefErr := webui.LoadDesktopLaunchPreferences(); prefErr != nil {
		fmt.Fprintf(os.Stderr, "metis desktop: preferences: %v (using Standard preset)\n", prefErr)
	} else if prefs.DefaultPreset != "" {
		presetName = prefs.DefaultPreset
		if presetName != "standard" {
			flags.agentProfile = presetName
		}
	}
	rt, err := setupRuntime(ctx, flags)
	if err != nil {
		return err
	}
	defer rt.Cleanup()

	addr := "127.0.0.1:" + opts.port
	serverCtx, cancelServer := context.WithCancel(ctx)
	defer cancelServer()
	shutdownToken := strings.TrimSpace(os.Getenv("METIS_DESKTOP_FRAME_TOKEN"))
	bindings := webui.RuntimeBindings{
		InitialSessionID:    rt.sessionID,
		ProviderName:        rt.providerName,
		PresetName:          presetName,
		FreshPermissionMode: rt.defaultPermissionMode,
		BuildProvider: func(providerName, model string) (*rtpkg.ProviderBuild, error) {
			cfg, _, err := config.Load()
			if err != nil {
				return nil, err
			}
			return rtpkg.BuildProvider(cfg, providerName, model)
		},
		SessionBoundary: rt.releaseSessionWork,
		SessionSwitch:   rt.rebindSessionAt,
		OpenWorkspace:   launchNativeDesktop,
		OpenPath:        desktop.OpenPath,
		Plugins:         rt.plugins,
		Roster:          rt.subAgentRoster,
		TraceAdapter:    rtpkg.CurrentTraceAdapter(),
		TraceStore:      rtpkg.CurrentTraceStore(),
	}
	// A regular `metis desktop --web` browser session has no frame token and
	// therefore no HTTP shutdown capability. The native shell supplies a fresh
	// high-entropy token per child launch and may cancel this server context.
	if shutdownToken != "" {
		bindings.ShutdownToken = shutdownToken
		bindings.Shutdown = cancelServer
	}
	srv := webui.NewServer(addr, rt.loop, rt.store, bindings)
	fmt.Fprintf(os.Stderr, "metis desktop --web: starting web UI on %s\n", addr)
	fmt.Fprintf(os.Stderr, "Open http://%s in your browser\n", addr)

	return srv.Run(serverCtx)
}

type desktopOptions struct {
	web  bool
	port string
	help bool
}

func parseDesktopOptions(args []string, getenv func(string) string) (desktopOptions, error) {
	opts := desktopOptions{port: "8080"}
	explicit := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--web":
			opts.web = true
		case "--port", "-p":
			if i+1 >= len(args) {
				return desktopOptions{}, fmt.Errorf("%s requires a port", args[i])
			}
			opts.port = args[i+1]
			opts.web = true
			explicit = true
			i++
		case "--help", "-h":
			opts.help = true
		default:
			return desktopOptions{}, fmt.Errorf("unknown desktop option: %s", args[i])
		}
	}
	if opts.web && !explicit && getenv != nil {
		if p := getenv("METIS_PORT"); p != "" {
			opts.port = p
		}
	}
	n, convErr := strconv.Atoi(opts.port)
	if convErr != nil || n < 1 || n > 65535 {
		return desktopOptions{}, fmt.Errorf("invalid desktop port %q (want 1-65535)", opts.port)
	}
	return opts, nil
}

var desktopHelp = `metis desktop — Launch the native Metis desktop app

Usage:
  metis desktop                    Open the native desktop client
  metis desktop --web              Start the browser UI on port 8080
  metis desktop --web --port 9090  Start the browser UI on a custom port

Flags:

  --web         Run the legacy browser UI instead of the native app
  --port, -p    Browser UI port (implies --web; env: METIS_PORT)
  --help, -h    Show this help
`
