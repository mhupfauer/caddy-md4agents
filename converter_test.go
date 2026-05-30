package md4agents

import (
	"strings"
	"testing"
)

func TestConverterBasics(t *testing.T) {
	m := &MarkdownForAgents{
		StripTags:    []string{"script", "style"},
		MainSelector: "main",
	}
	m.conv = m.buildConverter()

	html := `<html><body>
  <nav>Site nav</nav>
  <script>alert(1)</script>
  <main>
    <h1>Title</h1>
    <p>Hello <strong>world</strong>.</p>
    <p>Ignore: <span class="ad">buy me</span></p>
  </main>
  <footer>copy</footer>
</body></html>`

	md, err := m.convert([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "# Title") {
		t.Errorf("missing h1: %q", md)
	}
	if !strings.Contains(md, "**world**") {
		t.Errorf("missing bold: %q", md)
	}
	if strings.Contains(md, "alert(1)") {
		t.Errorf("script not stripped: %q", md)
	}
	if strings.Contains(md, "Site nav") {
		t.Errorf("nav leaked through main_selector: %q", md)
	}
}

func TestConverterStripSelectors(t *testing.T) {
	m := &MarkdownForAgents{
		StripSelectors: []string{".ad", "#cookie"},
	}
	m.conv = m.buildConverter()

	html := `<html><body>
  <p>Real content</p>
  <div class="ad">advertisement</div>
  <div id="cookie">accept cookies</div>
</body></html>`

	md, err := m.convert([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "Real content") {
		t.Errorf("real content missing: %q", md)
	}
	if strings.Contains(md, "advertisement") {
		t.Errorf(".ad not stripped: %q", md)
	}
	if strings.Contains(md, "accept cookies") {
		t.Errorf("#cookie not stripped: %q", md)
	}
}
