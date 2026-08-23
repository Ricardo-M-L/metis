package artifact

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/aymerick/douceur/css"
	cssparser "github.com/aymerick/douceur/parser"
	xhtml "golang.org/x/net/html"
)

var (
	// Active, embedded, form, media, and browser-context elements are removed
	// with their contents. Unknown/custom elements are converted to inert divs
	// so visible text survives without custom-element semantics.
	blockedElements = map[string]bool{
		"applet": true, "audio": true, "base": true, "button": true,
		"canvas": true, "embed": true, "form": true, "frame": true,
		"frameset": true, "iframe": true, "input": true, "link": true,
		"math": true, "meta": true, "noscript": true, "object": true,
		"portal": true, "script": true, "select": true, "slot": true,
		"source": true, "svg": true, "template": true, "textarea": true,
		"track": true, "video": true, "xmp": true,
	}
	allowedElements = map[string]bool{
		"a": true, "abbr": true, "address": true, "article": true,
		"aside": true, "b": true, "bdi": true, "bdo": true,
		"blockquote": true, "body": true, "br": true, "caption": true,
		"cite": true, "code": true, "col": true, "colgroup": true,
		"dd": true, "del": true, "details": true, "dfn": true,
		"div": true, "dl": true, "dt": true, "em": true,
		"figcaption": true, "figure": true, "footer": true, "h1": true,
		"h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"head": true, "header": true, "hgroup": true, "hr": true,
		"html": true, "i": true, "img": true, "ins": true, "kbd": true,
		"li": true, "main": true, "mark": true, "nav": true, "ol": true,
		"p": true, "pre": true, "q": true, "rp": true, "rt": true,
		"ruby": true, "s": true, "samp": true, "section": true,
		"small": true, "span": true, "strong": true, "style": true,
		"sub": true, "summary": true, "sup": true, "table": true,
		"tbody": true, "td": true, "tfoot": true, "th": true,
		"thead": true, "time": true, "title": true, "tr": true,
		"u": true, "ul": true, "var": true, "wbr": true,
	}
	fragmentHref = regexp.MustCompile(`^#[A-Za-z][A-Za-z0-9_.:-]*$`)
	cssProperty  = regexp.MustCompile(`^(?:--|-?[a-z])[a-z0-9-]*$`)
	cssDanger    = regexp.MustCompile(`(?i)(?:url\s*\(|image(?:-set)?\s*\(|cross-fade\s*\(|element\s*\(|expression\s*\(|javascript\s*:|behavior\s*:|-moz-binding\s*:|(?:https?|file|ftp|data)\s*:|//)`)
)

// SanitizeHTML reduces model-provided HTML to a static, network-free document.
// It retains ordinary semantic markup, locally-scoped fragment links, safe CSS
// declarations, and verified raster data images. The raw and rendered forms
// are both capped because parser normalization can expand the output.
func SanitizeHTML(raw string) (string, error) {
	if len(raw) > MaxHTMLBytes {
		return "", ErrTooLarge
	}
	doc, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("artifact: parse HTML: %w", err)
	}
	sanitizeChildren(doc)

	var out bytes.Buffer
	if err := xhtml.Render(&out, doc); err != nil {
		return "", fmt.Errorf("artifact: render sanitized HTML: %w", err)
	}
	if out.Len() > MaxHTMLBytes {
		return "", ErrTooLarge
	}
	return out.String(), nil
}

func sanitizeChildren(parent *xhtml.Node) {
	for child := parent.FirstChild; child != nil; {
		next := child.NextSibling
		if !sanitizeNode(child) {
			parent.RemoveChild(child)
		}
		child = next
	}
}

func sanitizeNode(node *xhtml.Node) bool {
	switch node.Type {
	case xhtml.CommentNode:
		return false
	case xhtml.ElementNode:
		tag := strings.ToLower(node.Data)
		if blockedElements[tag] {
			return false
		}
		if !allowedElements[tag] {
			// Retain the visible subtree but remove browser/custom-element
			// behavior and all attributes attached to the unknown element.
			node.Namespace = ""
			node.Data = "div"
			tag = "div"
		}
		node.Attr = sanitizeAttributes(tag, node.Attr)
		if tag == "style" && !sanitizeStyleElement(node) {
			return false
		}
	}
	sanitizeChildren(node)
	return true
}

func sanitizeAttributes(tag string, attrs []xhtml.Attribute) []xhtml.Attribute {
	out := attrs[:0]
	for _, attr := range attrs {
		name := strings.ToLower(attr.Key)
		if attr.Namespace != "" || strings.HasPrefix(name, "on") || name == "nonce" || name == "srcdoc" {
			continue
		}
		value := attr.Val
		allowed := name == "class" || name == "id" || name == "title" || name == "role" || name == "dir" || name == "lang" || name == "hidden" || strings.HasPrefix(name, "aria-")
		switch name {
		case "style":
			value = sanitizeCSSDeclarations(value)
			allowed = value != ""
		case "href":
			allowed = tag == "a" && fragmentHref.MatchString(strings.TrimSpace(value))
		case "src":
			allowed = tag == "img" && safeRasterDataImage(value)
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
		if !allowed {
			continue
		}
		attr.Namespace = ""
		attr.Key = name
		attr.Val = value
		out = append(out, attr)
	}
	return out
}

func sanitizeStyleElement(node *xhtml.Node) bool {
	var source strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == xhtml.TextNode {
			source.WriteString(child.Data)
		}
	}
	clean := sanitizeStylesheet(source.String())
	if clean == "" {
		return false
	}
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		node.RemoveChild(child)
		child = next
	}
	node.AppendChild(&xhtml.Node{Type: xhtml.TextNode, Data: clean})
	return true
}

func sanitizeStylesheet(source string) string {
	sheet, err := cssparser.Parse(source)
	if err != nil {
		return ""
	}
	safe := css.NewStylesheet()
	for _, rule := range sheet.Rules {
		// At-rules can import resources, declare fonts, or hide dangerous
		// declarations in nested grammars. Static artifacts do not need them.
		if rule == nil || rule.Kind != css.QualifiedRule || len(rule.Selectors) == 0 || strings.Contains(rule.Prelude, "\\") {
			continue
		}
		rule.Declarations = safeCSSDeclarations(rule.Declarations)
		if len(rule.Declarations) > 0 {
			safe.Rules = append(safe.Rules, rule)
		}
	}
	return strings.TrimSpace(safe.String())
}

func sanitizeCSSDeclarations(source string) string {
	declarations, err := cssparser.ParseDeclarations(source)
	if err != nil {
		return ""
	}
	safe := safeCSSDeclarations(declarations)
	parts := make([]string, 0, len(safe))
	for _, declaration := range safe {
		parts = append(parts, declaration.String())
	}
	return strings.Join(parts, " ")
}

func safeCSSDeclarations(declarations []*css.Declaration) []*css.Declaration {
	safe := make([]*css.Declaration, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration == nil {
			continue
		}
		property := strings.ToLower(strings.TrimSpace(declaration.Property))
		value := strings.TrimSpace(declaration.Value)
		if !cssProperty.MatchString(property) || value == "" || strings.Contains(property, "\\") || strings.Contains(value, "\\") || cssDanger.MatchString(value) {
			continue
		}
		switch property {
		case "behavior", "-moz-binding", "src":
			continue
		}
		declaration.Property = property
		declaration.Value = value
		safe = append(safe, declaration)
	}
	return safe
}

func safeRasterDataImage(value string) bool {
	trimmed := strings.TrimSpace(value)
	comma := strings.IndexByte(trimmed, ',')
	if comma < 0 {
		return false
	}
	header := strings.ToLower(strings.TrimSpace(trimmed[:comma]))
	wantMIME := ""
	switch header {
	case "data:image/png;base64":
		wantMIME = "image/png"
	case "data:image/jpeg;base64":
		wantMIME = "image/jpeg"
	case "data:image/gif;base64":
		wantMIME = "image/gif"
	case "data:image/webp;base64":
		wantMIME = "image/webp"
	default:
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed[comma+1:])
	if err != nil || len(decoded) == 0 {
		return false
	}
	return http.DetectContentType(decoded) == wantMIME
}
