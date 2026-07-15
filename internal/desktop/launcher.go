// Package desktop finds and launches the native Metis client.
package desktop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const metisReleasesURL = "https://github.com/Ricardo-M-L/metis/releases"

type launcher struct {
	goos         string
	goarch       string
	home         string
	cwd          string
	appOverride  string
	cliPath      string
	stat         func(string) (os.FileInfo, error)
	runCommand   func(string, ...string) error
	startCommand func(string, ...string) error
}

func systemLauncher() launcher {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	cliPath, _ := os.Executable()
	if abs, err := filepath.Abs(cliPath); err == nil {
		cliPath = abs
	}
	return launcher{
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
		home:         home,
		cwd:          cwd,
		appOverride:  os.Getenv("METIS_DESKTOP_APP"),
		cliPath:      cliPath,
		stat:         os.Stat,
		runCommand:   runCommand,
		startCommand: startCommand,
	}
}

// LaunchApp finds or installs the native Metis Desktop app and opens
// the given workspace via a process argument. Returns an error if the app
// cannot be found or launched.
func LaunchApp(workspace string) error {
	return systemLauncher().launchApp(workspace)
}

func (l launcher) launchApp(workspace string) error {
	switch l.goos {
	case "darwin":
		return l.launchMac(workspace)
	case "linux":
		return l.launchLinux(workspace)
	case "windows":
		return l.launchWindows(workspace)
	default:
		return fmt.Errorf("unsupported platform: %s", l.goos)
	}
}

// IsInstalled checks whether the native desktop app is already installed.
func IsInstalled() bool {
	return systemLauncher().isInstalled()
}

func (l launcher) isInstalled() bool { return l.findExistingAppPath() != "" }

// DownloadURL returns the release page for supported desktop platforms.
// The release workflow does not currently publish architecture-specific
// installers, so this must not point at an asset name that does not exist.
func DownloadURL() string {
	return systemLauncher().downloadURL()
}

func (l launcher) downloadURL() string {
	switch l.goos {
	case "darwin", "linux", "windows":
		return metisReleasesURL
	}
	return ""
}

func (l launcher) launchMac(workspace string) error {
	appPath := l.findExistingAppPath()
	if appPath == "" {
		return fmt.Errorf("metis desktop app not found; install from %s", l.downloadURL())
	}
	return l.openApp(appPath, workspace)
}

func (l launcher) launchLinux(workspace string) error {
	if appPath := l.findExistingAppPath(); appPath != "" {
		return l.openApp(appPath, workspace)
	}
	return fmt.Errorf("metis desktop app not found; install from %s", l.downloadURL())
}

func (l launcher) launchWindows(workspace string) error {
	if appPath := l.findExistingAppPath(); appPath != "" {
		return l.openApp(appPath, workspace)
	}
	return fmt.Errorf("metis desktop app not found; install from %s", l.downloadURL())
}

func (l launcher) findExistingAppPath() string {
	var candidates []string
	switch l.goos {
	case "darwin":
		candidates = []string{
			l.appOverride,
			filepath.Join(l.cwd, "metis-desktop", "build", "bin", "metis-desktop.app"),
			"/Applications/Metis.app",
			"/Applications/metis-desktop.app",
			filepath.Join(l.home, "Applications", "Metis.app"),
			filepath.Join(l.home, "Applications", "metis-desktop.app"),
		}
	case "linux":
		candidates = []string{
			l.appOverride,
			filepath.Join(l.cwd, "metis-desktop", "build", "bin", "metis-desktop"),
			"/usr/bin/metis-desktop",
			"/usr/local/bin/metis-desktop",
			filepath.Join(l.home, ".local", "bin", "metis-desktop"),
		}
	case "windows":
		candidates = []string{
			l.appOverride,
			filepath.Join(l.cwd, "metis-desktop", "build", "bin", "metis-desktop.exe"),
			filepath.Join(l.home, "AppData", "Local", "Metis", "Metis Desktop.exe"),
			"C:\\Program Files\\Metis\\Metis Desktop.exe",
			"C:\\Program Files (x86)\\Metis\\Metis Desktop.exe",
		}
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		info, err := l.stat(p)
		if err != nil {
			continue
		}
		if l.goos == "darwin" && info.IsDir() {
			return p
		}
		if l.goos == "linux" && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
		// Windows does not expose Unix executable bits through FileMode;
		// the .exe path and non-directory check are the relevant contract.
		if l.goos == "windows" && !info.IsDir() {
			return p
		}
	}
	return ""
}

func (l launcher) openApp(appPath, workspace string) error {
	appArgs := []string{"--workspace", workspace}
	if l.cliPath != "" {
		appArgs = append(appArgs, "--metis-bin", l.cliPath)
	}
	switch l.goos {
	case "darwin":
		// -n guarantees --args reaches this workspace even when another Metis
		// window is already running.
		commandArgs := append([]string{"-n", "-a", appPath, "--args"}, appArgs...)
		return l.runCommand("open", commandArgs...)
	case "linux":
		// The desktop process is long-lived. Starting it synchronously would
		// keep the invoking CLI blocked until the user closes the application.
		return l.startCommand(appPath, appArgs...)
	case "windows":
		commandArgs := append([]string{"/c", "start", "", appPath}, appArgs...)
		return l.runCommand("cmd", commandArgs...)
	}
	return fmt.Errorf("unsupported platform: %s", l.goos)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func startCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the process after the independently-running desktop app exits.
	go func() { _ = cmd.Wait() }()
	return nil
}
