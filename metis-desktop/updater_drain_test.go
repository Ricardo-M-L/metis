package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDesktopUpdaterDrainsOnlyVerifiedCandidateBeforeActivation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("simulated macOS activation requires atomic directory exchange, unavailable on Windows")
	}
	for _, stage := range []string{"success", "validation failure", "drain failure", "activation failure", "candidate disappeared", "context cancelled"} {
		t.Run(stage, func(t *testing.T) {
			appPath := filepath.Join(t.TempDir(), "METIS.app")
			writeTestDesktopBundle(t, appPath, "1.0.0", "current")
			u := updaterForDrainFixture(t)
			var order []string
			var candidate string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			u.validate = func(path, _ string) error {
				candidate = path
				order = append(order, "validate")
				if stage == "validation failure" {
					return errors.New("signature fixture rejected")
				}
				return nil
			}
			u.beforeActivate = func(ctx context.Context, gotPath string) error {
				order = append(order, "drain")
				if ctx == nil || gotPath != appPath {
					t.Fatalf("drain context/path = %v %q", ctx, gotPath)
				}
				current, err := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "metis-desktop"))
				if err != nil || string(current) != "current" {
					t.Fatalf("application activated before backend drain: %q, %v", current, err)
				}
				if stage == "drain failure" {
					return errors.New("backend fixture did not exit")
				}
				if stage == "activation failure" {
					// Refuse deletion of an unrelated directory at the rollback
					// location to simulate an activation failure after draining.
					if err := os.Mkdir(appPath+".previous", 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "candidate disappeared" {
					if err := os.RemoveAll(candidate); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "context cancelled" {
					cancel()
				}
				return nil
			}
			status, err := u.Install(ctx, "1.0.0", appPath)
			wantOrder := []string{"validate", "drain"}
			if stage == "validation failure" {
				wantOrder = []string{"validate"}
			}
			if !reflect.DeepEqual(order, wantOrder) {
				t.Fatalf("update order = %v, want %v", order, wantOrder)
			}
			current, readErr := os.ReadFile(filepath.Join(appPath, "Contents", "MacOS", "metis-desktop"))
			if stage == "success" {
				if err != nil || !status.Installed || readErr != nil || string(current) != "candidate" {
					t.Fatalf("successful drained install = %+v error:%v current:%q read:%v", status, err, current, readErr)
				}
				return
			}
			if err == nil || status.Installed || readErr != nil || string(current) != "current" {
				t.Fatalf("failed %s changed application: %+v error:%v current:%q read:%v", stage, status, err, current, readErr)
			}
			if stage == "drain failure" && !strings.Contains(err.Error(), "backend fixture did not exit") {
				t.Fatalf("lost backend drain failure: %v", err)
			}
			if stage == "context cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost update cancellation: %v", err)
			}
			if stage == "candidate disappeared" {
				if _, err := os.Lstat(appPath + ".previous"); !os.IsNotExist(err) {
					t.Fatalf("old bundle was not restored from rollback after rename failed: %v", err)
				}
			}
		})
	}
}

func updaterForDrainFixture(t *testing.T) desktopUpdater {
	t.Helper()
	archive := makeDesktopZip(t, "1.2.0", "candidate")
	sum := sha256.Sum256(archive)
	assetName := "metis-desktop-darwin-universal.zip"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/owner/repo/releases/latest":
			http.Redirect(w, r, server.URL+"/owner/repo/releases/tag/v1.2.0", http.StatusFound)
		case "/owner/repo/releases/tag/v1.2.0":
			w.WriteHeader(http.StatusOK)
		case "/owner/repo/releases/download/v1.2.0/" + assetName:
			_, _ = w.Write(archive)
		case "/owner/repo/releases/download/v1.2.0/" + assetName + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return desktopUpdater{webBase: server.URL, repo: "owner/repo", client: server.Client(), goos: "darwin", goarch: "arm64"}
}
