package webui

import (
	"archive/zip"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/artifact"
	"golang.org/x/net/html"
)

const (
	artifactPreviewTTL      = 2 * time.Minute
	maxArtifactPreviewBytes = 2 << 20
)

var (
	artifactCSSImportRE  = regexp.MustCompile(`(?is)@(?:import|namespace)[^;]*(?:;|$)`)
	artifactCSSURLRE     = regexp.MustCompile(`(?is)url\s*\([^)]*\)`)
	artifactCSSDangerRE  = regexp.MustCompile(`(?is)(?:expression\s*\(|javascript\s*:|behavior\s*:|-moz-binding\s*:)`)
	artifactFragmentHref = regexp.MustCompile(`^#[A-Za-z][A-Za-z0-9_.:-]*$`)
)

// artifactPreview launches a short-lived, capability-addressed HTTP origin.
// Artifact HTML is deliberately never served by the authenticated WebUI
// origin: even a future sanitizer regression therefore cannot grant it access
// to /api/* through same-origin browser credentials.
type artifactPreview struct {
	URL  string
	done <-chan struct{}
}

func startArtifactPreview(source []byte, ttl time.Duration) (*artifactPreview, error) {
	clean, err := sanitizeArtifactHTML(source)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, errors.New("artifact preview TTL must be positive")
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for artifact preview: %w", err)
	}
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(cryptorand.Reader, tokenBytes); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("create artifact preview capability: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	path := "/" + token
	host := listener.Addr().String()
	done := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setArtifactPreviewHeaders(w.Header())
		if r.Host != host || r.URL.Path != path || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(clean)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(clean)
		}
	})

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		timer := time.NewTimer(ttl)
		defer timer.Stop()
		go func() {
			_ = server.Serve(listener)
			close(done)
		}()
		<-timer.C
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	return &artifactPreview{URL: "http://" + host + path, done: done}, nil
}

func setArtifactPreviewHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store, max-age=0")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; connect-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")
	header.Set("Permissions-Policy", "accelerometer=(), autoplay=(), camera=(), display-capture=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}

// sanitizeArtifactHTML produces a static document. The preview origin also
// enforces a script-free, network-free CSP; sanitization is a second boundary
// that removes active elements, event handlers, navigable URLs, and CSS loads.
func sanitizeArtifactHTML(source []byte) ([]byte, error) {
	if len(source) == 0 {
		return nil, errors.New("artifact HTML is empty")
	}
	if len(source) > maxArtifactPreviewBytes {
		return nil, fmt.Errorf("artifact HTML exceeds %d bytes", maxArtifactPreviewBytes)
	}
	doc, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return nil, fmt.Errorf("parse artifact HTML: %w", err)
	}
	sanitizeArtifactNode(doc)
	var out bytes.Buffer
	if err := html.Render(&out, doc); err != nil {
		return nil, fmt.Errorf("render sanitized artifact HTML: %w", err)
	}
	return out.Bytes(), nil
}

var artifactDroppedElements = map[string]struct{}{
	"applet": {}, "audio": {}, "base": {}, "button": {}, "canvas": {},
	"embed": {}, "form": {}, "frame": {}, "frameset": {}, "iframe": {},
	"input": {}, "link": {}, "math": {}, "meta": {}, "object": {},
	"script": {}, "select": {}, "slot": {}, "source": {}, "svg": {},
	"template": {}, "textarea": {}, "track": {}, "video": {},
}

var artifactAllowedElements = map[string]struct{}{
	"a": {}, "abbr": {}, "address": {}, "article": {}, "aside": {},
	"b": {}, "bdi": {}, "bdo": {}, "blockquote": {}, "body": {}, "br": {},
	"caption": {}, "cite": {}, "code": {}, "col": {}, "colgroup": {},
	"dd": {}, "del": {}, "details": {}, "dfn": {}, "div": {}, "dl": {}, "dt": {},
	"em": {}, "figcaption": {}, "figure": {}, "footer": {}, "h1": {}, "h2": {},
	"h3": {}, "h4": {}, "h5": {}, "h6": {}, "head": {}, "header": {}, "hgroup": {},
	"hr": {}, "html": {}, "i": {}, "img": {}, "ins": {}, "kbd": {}, "li": {},
	"main": {}, "mark": {}, "nav": {}, "ol": {}, "p": {}, "pre": {}, "q": {},
	"rp": {}, "rt": {}, "ruby": {}, "s": {}, "samp": {}, "section": {}, "small": {},
	"span": {}, "strong": {}, "style": {}, "sub": {}, "summary": {}, "sup": {},
	"table": {}, "tbody": {}, "td": {}, "tfoot": {}, "th": {}, "thead": {},
	"time": {}, "title": {}, "tr": {}, "u": {}, "ul": {}, "var": {}, "wbr": {},
}

func sanitizeArtifactNode(parent *html.Node) {
	for node := parent.FirstChild; node != nil; {
		next := node.NextSibling
		switch node.Type {
		case html.CommentNode:
			parent.RemoveChild(node)
		case html.ElementNode:
			tag := strings.ToLower(node.Data)
			if _, drop := artifactDroppedElements[tag]; drop {
				parent.RemoveChild(node)
				node = next
				continue
			}
			if _, allowed := artifactAllowedElements[tag]; !allowed {
				// Unknown/custom elements become inert containers so their visible
				// text survives without retaining custom-element semantics.
				node.Namespace = ""
				node.Data = "div"
				tag = "div"
			}
			sanitizeArtifactAttrs(node, tag)
			if tag == "style" {
				for child := node.FirstChild; child != nil; child = child.NextSibling {
					if child.Type == html.TextNode {
						child.Data = sanitizeArtifactCSS(child.Data)
					}
				}
			}
			sanitizeArtifactNode(node)
		}
		node = next
	}
}

func sanitizeArtifactAttrs(node *html.Node, tag string) {
	attrs := node.Attr[:0]
	for _, attr := range node.Attr {
		name := strings.ToLower(attr.Key)
		if attr.Namespace != "" || strings.HasPrefix(name, "on") || name == "srcdoc" || name == "nonce" {
			continue
		}
		value := attr.Val
		allowed := name == "class" || name == "id" || name == "title" || name == "role" || name == "dir" || name == "lang" || strings.HasPrefix(name, "aria-")
		switch name {
		case "style":
			value = sanitizeArtifactCSS(value)
			allowed = strings.TrimSpace(value) != ""
		case "href":
			allowed = tag == "a" && artifactFragmentHref.MatchString(value)
		case "src":
			allowed = tag == "img" && safeArtifactImageDataURL(value)
		case "alt", "width", "height":
			allowed = tag == "img"
		case "colspan", "rowspan", "scope":
			allowed = tag == "td" || tag == "th"
		case "span":
			allowed = tag == "col" || tag == "colgroup"
		case "open":
			allowed = tag == "details"
		case "datetime":
			allowed = tag == "time" || tag == "ins" || tag == "del"
		}
		if allowed {
			attr.Namespace = ""
			attr.Key = name
			attr.Val = value
			attrs = append(attrs, attr)
		}
	}
	node.Attr = attrs
}

func sanitizeArtifactCSS(value string) string {
	value = artifactCSSImportRE.ReplaceAllString(value, "")
	value = artifactCSSURLRE.ReplaceAllString(value, "none")
	value = artifactCSSDangerRE.ReplaceAllString(value, "blocked(")
	return value
}

func safeArtifactImageDataURL(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	for _, prefix := range []string{
		"data:image/png;base64,",
		"data:image/jpeg;base64,",
		"data:image/gif;base64,",
		"data:image/webp;base64,",
	} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// artifacts returns the shared local Store. Tests may inject a private Store
// before serving; production lazily opens the METIS_HOME store so constructing
// a read-only Server has no filesystem side effect.
func (s *Server) artifacts() (*artifact.Store, error) {
	if s != nil && s.artifactStore != nil {
		return s.artifactStore, nil
	}
	return artifact.DefaultStore()
}

func (s *Server) artifactSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	requested := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	s.stateMu.RLock()
	active := s.activeSessionID
	s.stateMu.RUnlock()
	if requested == "" {
		requested = active
	}
	if !validSessionID(requested) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return "", false
	}
	if active != "" && requested != active {
		writeError(w, http.StatusConflict, "activate the session before accessing its artifacts")
		return "", false
	}
	return requested, true
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sessionID, ok := s.artifactSession(w, r)
	if !ok {
		return
	}
	store, err := s.artifacts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "artifact store unavailable")
		return
	}
	items, err := store.List(sessionID)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": items})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	tail := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/artifacts/"), "/")
	parts := strings.Split(tail, "/")
	if tail == "" || len(parts) > 2 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "invalid artifact path")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	sessionID, ok := s.artifactSession(w, r)
	if !ok {
		return
	}
	store, err := s.artifacts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "artifact store unavailable")
		return
	}

	if r.Method == http.MethodDelete && action == "" {
		if err := store.Delete(sessionID, id); err != nil {
			writeArtifactError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if action == "" {
		if r.Method == http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "HEAD is not supported for artifact metadata")
			return
		}
		manifest, err := store.Get(sessionID, id)
		if err != nil {
			writeArtifactError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"artifact": manifest})
		return
	}

	version, err := artifactVersionQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, versionMeta, err := store.ReadVersion(sessionID, id, version)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	manifest, err := store.Get(sessionID, id)
	if err != nil {
		writeArtifactError(w, err)
		return
	}

	switch action {
	case "preview":
		if r.Method == http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "HEAD is not supported for preview creation")
			return
		}
		preview, err := startArtifactPreview(body, artifactPreviewTTL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to prepare artifact preview")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"url": preview.URL, "version": versionMeta.Number})
	case "download":
		filename := fmt.Sprintf("artifact-%s-v%d.html", manifest.ID, versionMeta.Number)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case "export":
		filename := fmt.Sprintf("artifact-%s-v%d.zip", manifest.ID, versionMeta.Number)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := writeArtifactZIP(w, manifest, versionMeta, body); err != nil {
			return
		}
	default:
		writeError(w, http.StatusNotFound, "artifact action not found")
	}
}

func artifactVersionQuery(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("version"))
	if raw == "" {
		return 0, nil
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version < 1 {
		return 0, errors.New("version must be a positive integer")
	}
	return version, nil
}

func writeArtifactZIP(w io.Writer, manifest *artifact.Manifest, version *artifact.Version, body []byte) error {
	archive := zip.NewWriter(w)
	index, err := archive.Create("index.html")
	if err == nil {
		_, err = index.Write(body)
	}
	if err == nil {
		meta, marshalErr := json.MarshalIndent(map[string]any{"artifact": manifest, "exported_version": version}, "", "  ")
		if marshalErr != nil {
			err = marshalErr
		} else {
			var entry io.Writer
			entry, err = archive.Create("manifest.json")
			if err == nil {
				_, err = entry.Write(append(meta, '\n'))
			}
		}
	}
	closeErr := archive.Close()
	return errors.Join(err, closeErr)
}

func writeArtifactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		writeError(w, http.StatusNotFound, "artifact not found")
	case errors.Is(err, artifact.ErrOwnerMismatch):
		writeError(w, http.StatusForbidden, "artifact does not belong to the active session")
	case errors.Is(err, artifact.ErrInvalidID), errors.Is(err, artifact.ErrInvalidSession), errors.Is(err, artifact.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, "invalid artifact request")
	case errors.Is(err, artifact.ErrUnsafeFile):
		writeError(w, http.StatusConflict, "artifact failed integrity validation")
	default:
		writeError(w, http.StatusInternalServerError, "artifact operation failed")
	}
}
