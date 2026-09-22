package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/desktop"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/webui"
)

var launchNativeDesktop = desktop.LaunchApp

const desktopTotalAgentSlots = 12

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
	workspaceTrusted := currentWorkspaceTrusted()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("desktop: locate scheduler executable: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("desktop: determine scheduler workspace: %w", err)
	}
	turnParallelism := desktopWorkerParallelism(os.Getenv)
	// Lock files are owned by this Desktop backend and inherited only by its
	// isolated turn workers. OS advisory locks release on a worker crash; the
	// directory itself is removed once the Desktop server exits.
	subagentSlotDir, err := os.MkdirTemp("", "metis-desktop-agent-slots-")
	if err != nil {
		return fmt.Errorf("desktop: create sub-agent scheduler storage: %w", err)
	}
	defer os.RemoveAll(subagentSlotDir)
	bindings := webui.RuntimeBindings{
		InitialSessionID:        rt.sessionID,
		ProviderName:            rt.providerName,
		PresetName:              presetName,
		FreshSystemPromptKind:   rt.systemPromptKind,
		FreshPermissionMode:     rt.defaultPermissionMode,
		TrustSessionPermissions: rt.allowStoredSessionPermissions,
		TrustProviderConfig:     workspaceTrusted,
		SetPermissionMode: func(mode permission.Mode) error {
			return applyRuntimePermissionMode(rt, mode)
		},
		ComputerUse: rt.computerUseAction,
		PreflightPermissionMode: func(mode permission.Mode, prePlan string) error {
			return rtpkg.PreflightRestoredPermissionState(rt.sandbox, mode, prePlan)
		},
		BuildProvider: func(providerName, model string) (*rtpkg.ProviderBuild, error) {
			cfg, _, err := config.Load()
			if err != nil {
				return nil, err
			}
			if err := config.ApplyProviderPolicyForWorkspace(cfg, workspaceTrusted); err != nil {
				return nil, err
			}
			// Desktop has a separate, explicitly confirmed network Probe action.
			// Keep provider construction local so the settings Validate action and
			// model switching never emit an implicit HEAD request.
			return rtpkg.BuildProviderWithoutPreconnect(cfg, providerName, model)
		},
		SessionBoundary:      rt.releaseSessionWork,
		PrepareSessionSwitch: rt.prepareSessionRebindAt,
		OpenWorkspace:        launchNativeDesktop,
		OpenPath:             desktop.OpenPath,
		Plugins:              rt.plugins,
		Roster:               rt.subAgentRoster,
		TraceAdapter:         rtpkg.CurrentTraceAdapter(),
		TraceStore:           rtpkg.CurrentTraceStore(),
		Automations: &webui.AutomationOptions{
			Root:       filepath.Join(rt.cfg.Session.Dir, "cron"),
			Executable: executable,
			WorkDir:    workDir,
			Model:      rt.model,
		},
		// Every unattended foreground turn gets a private metis process. The
		// scheduler reserves a fixed total budget for roots and child agents,
		// while still serializing writes in one exact workspace.
		IsolatedTurns: &webui.IsolatedTurnOptions{
			Executable:          executable,
			MaxParallel:         turnParallelism,
			SubagentSlotDir:     subagentSlotDir,
			MaxSubagentSlots:    desktopChildAgentSlots(turnParallelism),
			MaxSubagentsPerRoot: 4,
		},
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

func desktopWorkerParallelism(getenv func(string) string) int {
	const defaultParallelism = 6
	if getenv == nil {
		return defaultParallelism
	}
	raw := strings.TrimSpace(getenv("METIS_DESKTOP_MAX_PARALLEL_TURNS"))
	if raw == "" {
		return defaultParallelism
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 8 {
		fmt.Fprintf(os.Stderr, "metis desktop: ignoring METIS_DESKTOP_MAX_PARALLEL_TURNS=%q (want 1..8)\n", raw)
		return defaultParallelism
	}
	return n
}

// desktopChildAgentSlots keeps the aggregate root + child agent budget fixed
// even when an advanced user raises or lowers the foreground worker setting.
// The default is six roots plus six child permits. A per-root roster cap still
// prevents one session from consuming the shared child pool.
func desktopChildAgentSlots(rootSlots int) int {
	if rootSlots < 1 {
		rootSlots = 1
	}
	if rootSlots >= desktopTotalAgentSlots {
		return 1
	}
	return desktopTotalAgentSlots - rootSlots
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
