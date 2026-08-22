//go:build !darwin || !cgo

package desktop

func platformClipboardFilePaths() ([]string, error) {
	return nil, ErrClipboardFilesUnsupported
}
