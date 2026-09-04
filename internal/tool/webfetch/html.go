package webfetch

import (
	"bytes"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// excludedHTMLElements do not contribute useful model content. Their entire
// subtrees are skipped before text normalization so script and CSS payloads
// cannot dominate the extracted document.
var excludedHTMLElements = map[string]struct{}{
	"audio":    {},
	"canvas":   {},
	"head":     {},
	"iframe":   {},
	"noscript": {},
	"script":   {},
	"style":    {},
	"svg":      {},
	"template": {},
	"video":    {},
}

var blockHTMLElements = map[string]struct{}{
	"article": {}, "aside": {}, "blockquote": {}, "dd": {}, "details": {},
	"dialog": {}, "dl": {}, "dt": {}, "fieldset": {}, "figcaption": {},
	"figure": {}, "footer": {}, "form": {}, "header": {}, "main": {},
	"menu": {}, "nav": {}, "ol": {}, "p": {}, "section": {}, "table": {},
	"tbody": {}, "tfoot": {}, "thead": {}, "tr": {}, "ul": {},
}

// htmlToMarkdown parses an HTML document with charset detection and renders a
// compact Markdown-oriented representation. It intentionally favors stable,
// model-useful semantics over source-fidelity.
func htmlToMarkdown(body []byte, contentType string, base *url.URL) (string, error) {
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return "", err
	}
	parsed, err := html.Parse(reader)
	if err != nil {
		return "", err
	}

	w := &markdownWriter{}
	if title := strings.TrimSpace(nodeText(findFirstElement(parsed, "title"))); title != "" {
		w.ensureBlank()
		w.raw("# " + title + "\n")
	}
	if bodyElement := findFirstElement(parsed, "body"); bodyElement != nil {
		for child := bodyElement.FirstChild; child != nil; child = child.NextSibling {
			w.walk(child, base, false)
		}
	}
	return w.String(), nil
}

// isHTMLResponse reports whether body should be run through HTML extraction.
// An explicit Content-Type wins; otherwise the first bytes are sniffed so
// incorrectly served HTML documents remain useful.
func isHTMLResponse(body []byte, contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	if mediaType == "" {
		sniffed := http.DetectContentType(body)
		if parsed, _, err := mime.ParseMediaType(sniffed); err == nil {
			mediaType = parsed
		} else {
			mediaType = sniffed
		}
	} else if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

func findFirstElement(n *html.Node, name string) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.Data == name {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findFirstElement(child, name); found != nil {
			return found
		}
	}
	return nil
}

// nodeText returns normalized text contained by n, excluding text inside
// elements that never contribute readable content.
func nodeText(n *html.Node) string {
	var out strings.Builder
	var visit func(*html.Node)
	visit = func(current *html.Node) {
		if current == nil {
			return
		}
		if current.Type == html.ElementNode {
			if _, excluded := excludedHTMLElements[current.Data]; excluded {
				return
			}
		}
		if current.Type == html.TextNode {
			fields := strings.Fields(current.Data)
			if len(fields) > 0 {
				if out.Len() > 0 {
					out.WriteByte(' ')
				}
				out.WriteString(strings.Join(fields, " "))
			}
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(n)
	return out.String()
}

type markdownWriter struct {
	builder strings.Builder
}

func (w *markdownWriter) String() string {
	return w.builder.String()
}

func (w *markdownWriter) raw(s string) {
	w.builder.WriteString(s)
}

func (w *markdownWriter) last() byte {
	s := w.builder.String()
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1]
}

func (w *markdownWriter) trimTrailingSpaces() {
	s := strings.TrimRight(w.builder.String(), " \t")
	w.builder.Reset()
	w.builder.WriteString(s)
}

func (w *markdownWriter) ensureWordSpace() {
	switch w.last() {
	case 0, ' ', '\n', '\r':
		return
	default:
		w.builder.WriteByte(' ')
	}
}

func (w *markdownWriter) ensureLine() {
	w.trimTrailingSpaces()
	if w.builder.Len() > 0 && w.last() != '\n' {
		w.builder.WriteByte('\n')
	}
}

func (w *markdownWriter) ensureBlank() {
	w.ensureLine()
	if w.builder.Len() > 0 {
		w.builder.WriteByte('\n')
	}
}

func (w *markdownWriter) text(s string) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return
	}
	w.ensureWordSpace()
	w.builder.WriteString(strings.Join(fields, " "))
}

func (w *markdownWriter) walk(n *html.Node, base *url.URL, preserveSpace bool) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.TextNode:
		if preserveSpace {
			w.builder.WriteString(n.Data)
			return
		}
		w.text(n.Data)
		return
	case html.CommentNode, html.DoctypeNode:
		return
	case html.ElementNode:
	default:
		return
	}

	tag := n.Data
	if _, excluded := excludedHTMLElements[tag]; excluded {
		return
	}

	switch tag {
	case "br":
		w.ensureLine()
	case "hr":
		w.ensureBlank()
		w.raw("---\n\n")
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := len(tag) - len("h")
		w.ensureBlank()
		w.raw(strings.Repeat("#", level) + " ")
		w.walkChildren(n, base, preserveSpace)
		w.ensureLine()
		w.builder.WriteByte('\n')
	case "pre":
		w.ensureBlank()
		w.raw("```\n")
		w.walkChildren(n, base, true)
		if w.last() != 0 && w.last() != '\n' {
			w.builder.WriteByte('\n')
		}
		w.raw("```\n\n")
	case "a":
		text := strings.TrimSpace(nodeText(n))
		if text == "" {
			text = "link"
		}
		w.ensureWordSpace()
		w.raw("[" + markdownText(text) + "](" + markdownURL(attrValue(n, "href"), base) + ")")
	case "img":
		alt := strings.TrimSpace(attrValue(n, "alt"))
		if alt == "" {
			alt = "image"
		}
		w.ensureWordSpace()
		w.raw("![" + markdownText(alt) + "](" + markdownURL(attrValue(n, "src"), base) + ")")
	case "li":
		w.ensureLine()
		w.raw(listMarker(n) + " ")
		w.walkChildren(n, base, preserveSpace)
		w.ensureLine()
	case "table":
		w.renderTable(n)
	default:
		if _, block := blockHTMLElements[tag]; block {
			w.ensureBlank()
		}
		w.walkChildren(n, base, preserveSpace)
		if _, block := blockHTMLElements[tag]; block {
			w.ensureLine()
			w.builder.WriteByte('\n')
		}
	}
}

func (w *markdownWriter) walkChildren(n *html.Node, base *url.URL, preserveSpace bool) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		w.walk(child, base, preserveSpace)
	}
}

func (w *markdownWriter) renderTable(table *html.Node) {
	w.ensureBlank()
	first := true
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "template":
				return
			case "tr":
				cells := tableRowCells(n)
				if len(cells) == 0 {
					return
				}
				w.ensureLine()
				w.raw("| " + strings.Join(cells, " | ") + " |")
				if first {
					w.ensureLine()
					w.raw("|" + strings.Repeat(" --- |", len(cells)))
					first = false
				}
				return
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(table)
	w.ensureLine()
	w.builder.WriteByte('\n')
}

func tableRowCells(row *html.Node) []string {
	cells := make([]string, 0, 2)
	for child := row.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}
		switch child.Data {
		case "td", "th":
			cells = append(cells, markdownText(strings.TrimSpace(nodeText(child))))
		}
	}
	return cells
}

func listMarker(li *html.Node) string {
	if li.Parent == nil || li.Parent.Type != html.ElementNode || li.Parent.Data != "ol" {
		return "-"
	}
	index := 1
	for sibling := li.PrevSibling; sibling != nil; sibling = sibling.PrevSibling {
		if sibling.Type == html.ElementNode && sibling.Data == "li" {
			index++
		}
	}
	return strconv.Itoa(index) + "."
}

func attrValue(n *html.Node, name string) string {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, name) {
			return attr.Val
		}
	}
	return ""
}

func markdownText(s string) string {
	replacer := strings.NewReplacer("[", "\\[", "]", "\\]", "|", "\\|")
	return replacer.Replace(s)
}

func markdownURL(value string, base *url.URL) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace(value)
	}
	if base != nil {
		parsed = base.ResolveReference(parsed)
	}
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace(parsed.String())
}
