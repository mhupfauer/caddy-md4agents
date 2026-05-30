package md4agents

import (
	"strings"

	"github.com/JohannesKaufmann/dom"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
)

// buildConverter constructs a thread-safe converter configured with the
// module's strip/main settings. The returned converter is safe for concurrent
// use across goroutines, matching the html-to-markdown/v2 contract.
func (m *MarkdownForAgents) buildConverter() *converter.Converter {
	conv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(
				commonmark.WithHeadingStyle(commonmark.HeadingStyleATX),
			),
			strikethrough.NewStrikethroughPlugin(),
			table.NewTablePlugin(),
		),
		converter.WithEscapeMode(converter.EscapeModeSmart),
	)

	for _, tag := range m.StripTags {
		conv.Register.TagType(strings.ToLower(tag),
			converter.TagTypeRemove, converter.PriorityStandard)
	}

	if len(m.StripSelectors) > 0 || m.MainSelector != "" {
		selectors := append([]string(nil), m.StripSelectors...)
		mainSel := m.MainSelector
		conv.Register.PreRenderer(func(ctx converter.Context, doc *html.Node) {
			if mainSel != "" {
				if node := findFirst(doc, parseSelector(mainSel)); node != nil {
					// Replace doc body with the main node so only it gets converted.
					promoteToRoot(doc, node)
				}
			}
			for _, sel := range selectors {
				s := parseSelector(sel)
				for _, n := range findAll(doc, s) {
					dom.RemoveNode(n)
				}
			}
		}, converter.PriorityStandard)
	}

	return conv
}

// convert turns an HTML byte slice into Markdown.
func (m *MarkdownForAgents) convert(htmlBytes []byte) (string, error) {
	return m.conv.ConvertString(string(htmlBytes))
}

// --- Tiny selector engine -----------------------------------------------------
//
// We only support tag, .class and #id (plus a comma-separated list of those).
// Going further would mean pulling in goquery / cascadia, which doubles the
// binary size for negligible benefit — the strip list is meant for a handful
// of obvious wrappers (nav, .ad, #cookie-banner), not arbitrary CSS.

type selector struct {
	tag   string // "" matches any tag
	class string
	id    string
}

func parseSelector(raw string) selector {
	s := selector{}
	raw = strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(raw, "."):
		s.class = raw[1:]
	case strings.HasPrefix(raw, "#"):
		s.id = raw[1:]
	default:
		s.tag = strings.ToLower(raw)
	}
	return s
}

func (s selector) matches(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if s.tag != "" && n.Data != s.tag {
		return false
	}
	if s.id != "" && dom.GetAttributeOr(n, "id", "") != s.id {
		return false
	}
	if s.class != "" {
		for _, c := range strings.Fields(dom.GetAttributeOr(n, "class", "")) {
			if c == s.class {
				return true
			}
		}
		return false
	}
	return true
}

// findAll returns only top-most matches: once a node matches we don't recurse
// into its subtree. That keeps the caller free to RemoveNode each result
// without dragging in nested matches whose Parent pointer is now stale.
func findAll(root *html.Node, s selector) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if s.matches(n) {
			out = append(out, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func findFirst(root *html.Node, s selector) *html.Node {
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if s.matches(n) {
			found = n
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(root)
	return found
}

// promoteToRoot reparents `keep` directly under `doc`, dropping siblings. We
// keep the document/html/head wrappers intact so the converter still sees a
// valid tree.
func promoteToRoot(doc, keep *html.Node) {
	body := findFirst(doc, selector{tag: "body"})
	if body == nil {
		return
	}
	// Detach keep from its current parent.
	dom.RemoveNode(keep)
	// Clear body children and append keep.
	for c := body.FirstChild; c != nil; {
		next := c.NextSibling
		dom.RemoveNode(c)
		c = next
	}
	body.AppendChild(keep)
}
