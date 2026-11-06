// Package blob stores immutable content-addressed session attachments.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxBytes bounds attachment allocation on both ingestion and resume.
const MaxBytes = 32 << 20

func Sum(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func Read(dir, hash string) ([]byte, error) {
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("attachment: invalid hash")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("attachment %s: %w", hash, err)
	}
	defer root.Close()
	file, err := root.Open(hash)
	if err != nil {
		return nil, fmt.Errorf("attachment %s: %w", hash, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, fmt.Errorf("attachment %s: invalid file or exceeds %d bytes", hash, MaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("attachment %s: exceeds %d bytes", hash, MaxBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("attachment %s: %w", hash, err)
	}
	if Sum(data) != hash {
		return nil, fmt.Errorf("attachment %s: checksum mismatch", hash)
	}
	return data, nil
}

func Put(dir string, data []byte) (string, error) {
	if len(data) > MaxBytes {
		return "", fmt.Errorf("attachment exceeds %d bytes", MaxBytes)
	}
	hash := Sum(data)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if existing, err := Read(dir, hash); err == nil && string(existing) == string(data) {
		return hash, nil
	}
	f, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(name, filepath.Join(dir, hash)); err != nil {
		return "", err
	}
	// Commit directory membership before a transcript can reference this blob.
	directory, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hash, nil
}
