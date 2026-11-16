// Live smoke launches the built CLI with one synthetic prompt and an isolated
// home. It reuses only the configured local gateway's endpoint-bound API key;
// no credential is printed or copied into the fixture files.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
)

type profile struct {
	Transport string `toml:"transport"`
	BaseURL   string `toml:"base_url"`
	Model     string `toml:"model"`
	KeyEnv    string `toml:"api_key_env"`
	Key       string `toml:"api_key"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	binaryFlag := flag.String("binary", "", "newly built Metis executable")
	outFlag := flag.String("out", "", "new isolated evidence directory (must not exist)")
	flag.Parse()
	if *binaryFlag == "" || *outFlag == "" {
		return fmt.Errorf("--binary and --out are required; this opt-in test calls the configured local model gateway")
	}
	var settings struct {
		Provider struct {
			Default string             `toml:"default"`
			Custom  map[string]profile `toml:"custom"`
		} `toml:"provider"`
	}
	if _, err := toml.DecodeFile(filepath.Join(config.Home(), "config.toml"), &settings); err != nil {
		return err
	}
	name := settings.Provider.Default
	p, ok := settings.Provider.Custom[name]
	if !ok || p.Transport != "anthropic_messages" {
		return fmt.Errorf("this bounded smoke requires the selected custom Anthropic-compatible local gateway")
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return fmt.Errorf("selected profile is not a plain local gateway URL")
	}
	key := os.Getenv(p.KeyEnv)
	if key == "" {
		key, err = auth.GetAPIKeyForEndpoint(name, p.Transport, p.BaseURL, false)
		if err != nil {
			return fmt.Errorf("configured endpoint-bound credential is not usable; no live request made")
		}
	}
	if key == "" {
		key = p.Key
	}
	if key == "" {
		return fmt.Errorf("no usable credential; no live request made")
	}
	binary, err := filepath.Abs(*binaryFlag)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(*outFlag)
	if err != nil {
		return err
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	home, workspace := filepath.Join(out, "home"), filepath.Join(out, "workspace")
	metisHome := filepath.Join(home, ".metis")
	for _, path := range []string{metisHome, workspace} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	// JSON-quoted strings are valid TOML strings and cannot inject new fields.
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	body := "[provider]\ndefault = \"live-smoke\"\n[provider.custom.live-smoke]\n" +
		"transport = \"anthropic_messages\"\nbase_url = " + quote(p.BaseURL) + "\nmodel = " + quote(p.Model) +
		"\napi_key_env = \"METIS_LIVE_SMOKE_KEY\"\nmax_tokens = 1024\ncontext_window = 65536\ntimeout_seconds = 45\n[permission]\nmode = \"dontAsk\"\n"
	if err := os.WriteFile(filepath.Join(metisHome, "config.toml"), []byte(body), 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "run", "--provider", "live-smoke", "--bare", "--simple", "--tools", "MetisInfo", "--max-iter", "1", "--max-budget-usd", "0.1", "--cache-ttl", "off", "--metrics-log", filepath.Join(out, "metrics.jsonl"), "--no-markdown", "Reply with exactly METIS_LIVE_SMOKE_OK. Do not call any tools.")
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + home, "METIS_HOME=" + metisHome, "METIS_LIVE_SMOKE_KEY=" + key, "METIS_AUTO_MEMORY=0", "METIS_CATALOG_DISABLE=1", "METIS_NO_UPDATE_CHECK=1", "LANG=en_US.UTF-8", "TERM=dumb"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	runErr := cmd.Run()
	matched := strings.Contains(stdout.String(), "METIS_LIVE_SMOKE_OK")
	for file, text := range map[string]string{"stdout.txt": stdout.String(), "stderr.txt": stderr.String()} {
		if err := os.WriteFile(filepath.Join(out, file), []byte(strings.ReplaceAll(text, key, "[REDACTED]")), 0600); err != nil {
			return err
		}
	}
	result := map[string]any{"provider": name, "model": p.Model, "matched": matched, "elapsed_ms": time.Since(started).Milliseconds(), "exit_ok": runErr == nil, "timed_out": ctx.Err() != nil}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "result.json"), encoded, 0600); err != nil {
		return err
	}
	fmt.Println(string(encoded))
	if runErr != nil || !matched {
		return fmt.Errorf("live CLI smoke did not complete successfully; see redacted evidence")
	}
	return nil
}
