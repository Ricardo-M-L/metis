package session

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/sessionfiles/blob"
	_ "golang.org/x/image/webp"
)

// TraceImage is a stable session-owned attachment reference. No payload, local
// path or untrusted remote URL is stored in the trace. Name is derived because
// provider-neutral image blocks do not currently preserve upload filenames.
type TraceImage struct {
	MediaType      string `json:"mediaType"`
	AttachmentHash string `json:"attachmentHash,omitempty"`
	Name           string `json:"name,omitempty"`
	Bytes          int    `json:"bytes,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	URL            string `json:"url,omitempty"`
	Available      bool   `json:"available"`
	Limitation     string `json:"limitation,omitempty"`
}

// DescribeTraceImage extracts bounded raster metadata from verified bytes.
// DecodeConfig reads only headers; rendering never accepts active SVG content.
func DescribeTraceImage(data []byte, hash string) (TraceImage, error) {
	img := TraceImage{AttachmentHash: hash, Bytes: len(data)}
	if len(data) > blob.MaxBytes {
		return img, fmt.Errorf("image exceeds attachment size limit")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return img, fmt.Errorf("unsupported or invalid raster image")
	}
	switch format {
	case "png", "gif", "jpeg", "webp":
		img.MediaType = "image/" + format
	default:
		return img, fmt.Errorf("unsupported raster image format")
	}
	img.Width, img.Height = cfg.Width, cfg.Height
	ext := format
	if ext == "jpeg" {
		ext = "jpg"
	}
	short := hash
	if len(short) > 12 {
		short = short[:12]
	}
	img.Name = "attachment-" + short + "." + ext
	img.Available = true
	return img, nil
}

// TraceImagesFromBlocks derives references without writing. It is also used
// when an embedder has not bound a session store yet; payloads remain excluded.
func TraceImagesFromBlocks(blocks []llm.ContentBlock) []TraceImage {
	var out []TraceImage
	for _, block := range blocks {
		if block.Synthetic || (block.Type != "image" && block.Type != "image_url" && block.Type != "input_image") {
			continue
		}
		img := TraceImage{MediaType: block.MediaType, AttachmentHash: block.AttachmentHash}
		if block.Data != "" && len(block.Data) <= base64.StdEncoding.EncodedLen(blob.MaxBytes) {
			if data, err := base64.StdEncoding.DecodeString(block.Data); err == nil {
				described, describeErr := DescribeTraceImage(data, blob.Sum(data))
				if describeErr == nil {
					img = described
				} else {
					img.Limitation = describeErr.Error()
				}
			} else {
				img.Limitation = "image data is not valid base64"
			}
		} else if block.Data != "" {
			img.Limitation = "image exceeds attachment size limit"
		}
		if img.Limitation == "" {
			img.Limitation = "attachment storage is unavailable"
		}
		img.Available = false
		out = append(out, img)
	}
	return out
}

// PersistTraceImages commits submitted images before recording references.
// Errors preserve a non-previewable metadata entry rather than losing the
// attachment or making the trace appear to have a working preview.
func (s *Store) PersistTraceImages(id string, blocks []llm.ContentBlock) ([]TraceImage, error) {
	if s == nil || !validTraceSessionID(id) {
		return TraceImagesFromBlocks(blocks), fmt.Errorf("invalid attachment session")
	}
	var out []TraceImage
	var errs []error
	for _, block := range blocks {
		if block.Synthetic || (block.Type != "image" && block.Type != "image_url" && block.Type != "input_image") {
			continue
		}
		img := TraceImage{MediaType: block.MediaType, AttachmentHash: block.AttachmentHash}
		var data []byte
		var err error
		switch {
		case block.Data != "":
			if len(block.Data) > base64.StdEncoding.EncodedLen(blob.MaxBytes) {
				err = fmt.Errorf("image exceeds attachment size limit")
			} else {
				data, err = base64.StdEncoding.DecodeString(block.Data)
			}
		case block.AttachmentHash != "":
			data, err = s.ReadTraceImage(id, block.AttachmentHash)
		default:
			err = fmt.Errorf("image bytes are unavailable; remote images are not fetched")
		}
		if err == nil {
			var described TraceImage
			described, err = DescribeTraceImage(data, blob.Sum(data))
			if err == nil {
				img = described
				if info, statErr := os.Lstat(s.attachmentDir(id)); statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
					err = fmt.Errorf("invalid attachment directory")
				}
			}
			if err == nil {
				img.AttachmentHash, err = blob.Put(s.attachmentDir(id), data)
			}
		}
		if err != nil {
			img.Available = false
			// Errors from blob.Read contain local paths; never expose them.
			img.Limitation = "image attachment is missing or cannot be safely decoded"
			errs = append(errs, fmt.Errorf("trace image: unavailable"))
		}
		out = append(out, img)
	}
	return out, errors.Join(errs...)
}

// ReadTraceImage reads only a checksum-verified blob under this exact session.
// The HTTP layer additionally verifies that the session references this hash.
func (s *Store) ReadTraceImage(id, hash string) ([]byte, error) {
	if s == nil || !validTraceSessionID(id) || strings.ToLower(hash) != hash {
		return nil, fmt.Errorf("invalid attachment identity")
	}
	info, err := os.Lstat(s.attachmentDir(id))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid attachment directory")
	}
	return blob.Read(s.attachmentDir(id), hash)
}

// TraceImageExists is the cheap polling check for already-recorded metadata.
// Opening a preview still performs the complete bounded checksum read.
func (s *Store) TraceImageExists(id, hash string, size int) bool {
	if s == nil || !validTraceSessionID(id) || strings.ToLower(hash) != hash {
		return false
	}
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 {
		return false
	}
	dir, err := os.Lstat(s.attachmentDir(id))
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
		return false
	}
	file, err := os.Lstat(filepath.Join(s.attachmentDir(id), hash))
	return err == nil && file.Mode().IsRegular() && file.Size() <= blob.MaxBytes && (size <= 0 || file.Size() == int64(size))
}
