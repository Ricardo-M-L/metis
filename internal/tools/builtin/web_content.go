package builtin

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// webTokenEstimate uses a local conservative approximation (ASCII 3 chars per
// token, non-ASCII 1 rune per token); it is not a provider tokenizer count.
func webTokenEstimate(s string) int {
	units := 0
	for _, r := range s {
		if r < 128 {
			units++
		} else {
			units += 3
		}
	}
	return (units + 2) / 3
}

func webBudget(s string, tokens int) string {
	if tokens <= 0 {
		return ""
	}
	if webTokenEstimate(s) <= tokens {
		return s
	}
	const suffix = "\n[output budget reached]"
	if webTokenEstimate(suffix) > tokens {
		return ""
	}
	remaining := tokens - webTokenEstimate(suffix)
	used, n := 0, 0
	for i, r := range s {
		cost := 1
		if r >= 128 {
			cost = 3
		}
		if used+cost > remaining*3 {
			break
		}
		used += cost
		n = i + utf8.RuneLen(r)
	}
	// Never leave a partial URL/Markdown link in the model-facing preview.
	prefix := s[:n]
	if i := strings.LastIndex(prefix, "["); i >= 0 && strings.Count(prefix[i:], "(") > strings.Count(prefix[i:], ")") {
		prefix = prefix[:i]
	}
	if i := strings.LastIndex(prefix, "http"); i >= 0 && !strings.ContainsAny(prefix[i:], " \t\n)") {
		prefix = prefix[:i]
	}
	return prefix + suffix
}

func webOutputBudget(in map[string]any) int {
	n := intArg(in, "max_output_tokens", 4096)
	if n < 1024 {
		n = 1024
	}
	if n > 16384 {
		n = 16384
	}
	return n
}

// htmlMarkdown is deterministic and local-only. Do not select only <article>:
// docs, repository browsers and API references often keep vital content outside
// it. Remove explicit non-content/hidden nodes and retain the remaining body.
func nodeTextExact(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if !htmlNodeVisible(n) {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

const maxHTMLTableDepth = 8

func renderHTMLTable(n *html.Node, source string, tableDepth int) string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if !htmlNodeVisible(n) {
			return
		}
		if n.Type == html.ElementNode && n.Data == "tr" {
			var row []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					if !htmlNodeVisible(c) {
						continue
					}
					// Render the original subtree, not a reparsed fragment: cell
					// and row ancestors must retain their visibility semantics.
					text := htmlMarkdownNode(c, source, tableDepth+1, true)
					text = strings.ReplaceAll(text, "|", "\\|")
					text = strings.Join(strings.Fields(text), " ")
					row = append(row, text)
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	var b strings.Builder
	b.WriteByte('\n')
	for i, r := range rows {
		for len(r) < cols {
			r = append(r, "")
		}
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
		if i == 0 {
			b.WriteString("|" + strings.Repeat(" --- |", cols) + "\n")
		}
	}
	return b.String()
}
func renderNodeHTML(n *html.Node) string {
	var b strings.Builder
	_ = html.Render(&b, n)
	return b.String()
}

// htmlNodeVisible is shared by the document and table walkers. Returning early
// at each ancestor keeps hidden sections/rows/cells out of nested projections.
func htmlNodeVisible(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return true
	}
	switch n.Data {
	case "script", "style", "noscript", "template", "head", "iframe", "object", "embed":
		return false
	}
	for _, a := range n.Attr {
		v := strings.ToLower(strings.Join(strings.Fields(a.Val), ""))
		if a.Key == "hidden" || (a.Key == "aria-hidden" && v == "true") || (a.Key == "style" && (strings.Contains(v, "display:none") || strings.Contains(v, "visibility:hidden"))) {
			return false
		}
	}
	return true
}

func htmlMarkdown(body, source string) string {
	return htmlMarkdownAtDepth(body, source, 0)
}

// Nested tables beyond the small conversion limit are flattened by the normal
// walker, preserving links/text without further serialization or reparsing.
func htmlMarkdownAtDepth(body, source string, tableDepth int) string {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return body
	}
	return htmlMarkdownNode(doc, source, tableDepth, false)
}

// childrenOnly renders an existing table cell without losing its ancestor
// filtering or introducing the cell's structural separator into its contents.
func htmlMarkdownNode(doc *html.Node, source string, tableDepth int, childrenOnly bool) string {
	base, _ := url.Parse(source)
	title, hasH1 := "", false
	var scan func(*html.Node)
	scan = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "title" {
				title = nodeText(n)
			}
			if n.Data == "h1" {
				hasH1 = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			scan(c)
		}
	}
	scan(doc)
	var b strings.Builder
	if !hasH1 && title != "" {
		fmt.Fprintf(&b, "# %s\n\n", title)
	}
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, pre bool) {
		if !htmlNodeVisible(n) {
			return
		}
		if n.Type == html.ElementNode {
			// Special blocks are rendered atomically to preserve literal code and
			// valid Markdown table structure (including pipe escaping).
			if n.Data == "pre" {
				text := nodeTextExact(n)
				fence := "```"
				for strings.Contains(text, fence) {
					fence += "`"
				}
				fmt.Fprintf(&b, "\n%s\n%s\n%s\n", fence, text, fence)
				return
			}
			if n.Data == "table" && tableDepth < maxHTMLTableDepth {
				b.WriteString(renderHTMLTable(n, source, tableDepth))
				return
			}
			switch n.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				fmt.Fprintf(&b, "\n\n%s ", strings.Repeat("#", int(n.Data[1]-'0')))
			case "p", "div", "section", "article", "main", "ul", "ol", "table", "tr":
				b.WriteString("\n")
			case "br":
				b.WriteString("\n")
			case "li":
				b.WriteString("\n- ")
			case "th", "td":
				b.WriteString(" | ")
			case "pre":
				b.WriteString("\n```\n")
				pre = true
			case "code":
				if !pre {
					b.WriteString("`")
				}
			case "a":
				b.WriteString("[")
			}
		}
		if n.Type == html.TextNode {
			if pre {
				b.WriteString(n.Data)
			} else {
				text := strings.Join(strings.Fields(n.Data), " ")
				if text != "" {
					b.WriteString(text)
					b.WriteByte(' ')
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, pre)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a":
				href := ""
				for _, a := range n.Attr {
					if a.Key == "href" {
						href = a.Val
					}
				}
				u, e := url.Parse(href)
				if e == nil && base != nil {
					u = base.ResolveReference(u)
				}
				if e == nil && (u.Scheme == "http" || u.Scheme == "https") {
					fmt.Fprintf(&b, "](%s)", u.String())
				} else {
					b.WriteString("]")
				}
			case "pre":
				b.WriteString("\n```\n")
			case "code":
				if !pre {
					b.WriteString("`")
				}
			case "p", "div", "section", "article", "main", "li", "tr", "table", "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString("\n")
			}
		}
	}
	if childrenOnly {
		for c := doc.FirstChild; c != nil; c = c.NextSibling {
			walk(c, false)
		}
	} else {
		walk(doc, false)
	}
	out := strings.TrimSpace(b.String())
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}
