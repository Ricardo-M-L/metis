package computeruse

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxArchiveEntries = 1024

func extractArtifact(ctx context.Context, artifact, destination string, release Release) error {
	if release.Format == "binary" {
		filename := filepath.Join(destination, filepath.FromSlash(release.BinaryPath))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return err
		}
		return copyRegular(ctx, artifact, filename, 0500, maxArtifactBytes)
	}
	budget := maxArtifactBytes
	entries := 0
	write := func(name string, mode os.FileMode, size int64, input io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > maxArchiveEntries {
			return errors.New("computer-use archive contains too many entries")
		}
		if mode.IsDir() {
			name = strings.TrimSuffix(name, "/")
		}
		if !safeRelative(name) {
			return fmt.Errorf("unsafe computer-use archive path %q", name)
		}
		if !mode.IsRegular() && !mode.IsDir() {
			return errors.New("computer-use archive contains a symlink or unsupported file type")
		}
		filename := filepath.Join(destination, filepath.FromSlash(name))
		if mode.IsDir() {
			return os.MkdirAll(filename, 0700)
		}
		if size < 0 || size > budget {
			return errors.New("computer-use archive exceeds extracted size limit")
		}
		budget -= size
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			return err
		}
		permissions := os.FileMode(0400)
		if mode.Perm()&0111 != 0 {
			permissions = 0500
		}
		output, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, permissions)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(output, io.LimitReader(contextReader{ctx, input}, size+1))
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != size {
			return errors.New("computer-use archive entry size mismatch")
		}
		return nil
	}
	switch release.Format {
	case "tar.gz":
		file, err := openRegular(artifact)
		if err != nil {
			return err
		}
		defer file.Close()
		compressed, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader := tar.NewReader(compressed)
		for {
			header, err := reader.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
				return errors.New("computer-use archive contains a link or unsupported entry")
			}
			if err := write(header.Name, header.FileInfo().Mode(), header.Size, reader); err != nil {
				return err
			}
		}
	case "zip":
		reader, err := zip.OpenReader(artifact)
		if err != nil {
			return err
		}
		defer reader.Close()
		for _, entry := range reader.File {
			if entry.UncompressedSize64 > uint64(maxArtifactBytes) {
				return errors.New("computer-use zip entry exceeds extracted size limit")
			}
			input, err := entry.Open()
			if err != nil {
				return err
			}
			err = write(entry.Name, entry.Mode(), int64(entry.UncompressedSize64), input)
			closeErr := input.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported computer-use archive format %q", release.Format)
	}
}
