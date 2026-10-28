package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDesktopAtomicSwapPreservesBothObjects(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("atomic Desktop swap is unsupported on this OS")
	}
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			left, right := filepath.Join(root, "left"), filepath.Join(root, "right")
			directory := kind == "directory"
			desktopSwapTestObject(t, left, "original-left", directory)
			desktopSwapTestObject(t, right, "original-right", directory)
			beforeLeft, err := os.Stat(left)
			if err != nil {
				t.Fatal(err)
			}
			beforeRight, err := os.Stat(right)
			if err != nil {
				t.Fatal(err)
			}
			if err := desktopAtomicSwap(left, right); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				path    string
				content string
				before  os.FileInfo
			}{{left, "original-right", beforeRight}, {right, "original-left", beforeLeft}} {
				after, err := os.Stat(item.path)
				if err != nil || !os.SameFile(item.before, after) {
					t.Fatalf("swapped object at %s lost its identity: %v", item.path, err)
				}
				payload := item.path
				if directory {
					payload = filepath.Join(payload, "payload")
				}
				contents, err := os.ReadFile(payload)
				if err != nil || string(contents) != item.content {
					t.Fatalf("swapped payload = %q, %v; want %q", contents, err, item.content)
				}
			}
		})
	}
}

func TestDesktopAtomicSwapMissingPeerPreservesLeft(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("atomic Desktop swap is unsupported on this OS")
	}
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			left, missing := filepath.Join(root, "left"), filepath.Join(root, "missing")
			directory := kind == "directory"
			desktopSwapTestObject(t, left, "preserved-left", directory)
			before, err := os.Stat(left)
			if err != nil {
				t.Fatal(err)
			}
			if err := desktopAtomicSwap(left, missing); err == nil {
				t.Fatal("atomic swap unexpectedly accepted a missing peer")
			}
			after, err := os.Stat(left)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("failed swap changed the left object: %v", err)
			}
			payload := left
			if directory {
				payload = filepath.Join(payload, "payload")
			}
			contents, err := os.ReadFile(payload)
			if err != nil || string(contents) != "preserved-left" {
				t.Fatalf("failed swap changed the left payload: %q, %v", contents, err)
			}
			if _, err := os.Lstat(missing); !os.IsNotExist(err) {
				t.Fatalf("failed swap created the missing peer: %v", err)
			}
		})
	}
}

func desktopSwapTestObject(t *testing.T, path, contents string, directory bool) {
	t.Helper()
	if directory {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(path, "payload")
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
