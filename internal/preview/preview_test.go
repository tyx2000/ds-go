package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webhtml "golang.org/x/net/html"
)

func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 12, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 12; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 20), uint8(y * 30), 128, 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func sourceFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTextPagesSearchEncodingAndCancellation(t *testing.T) {
	p := sourceFile(t, "source.txt", []byte("one\ntwo\nthree two\nfour\n"))
	r, err := Open(context.Background(), ".", Options{Path: p, Line: 2, Limit: 2, Search: "two"})
	if err != nil || r.TotalLines != 4 || r.NextLine != 4 || len(r.Lines) != 2 || r.Lines[0].Number != 2 || len(r.Matches) != 2 {
		t.Fatalf("page: %+v %v", r, err)
	}
	if text, err := decodeText([]byte{0xff, 0xfe, 'A', 0, 'B', 0}); err != nil || text != "AB" {
		t.Fatalf("UTF16: %q %v", text, err)
	}
	for _, bad := range [][]byte{{0xff}, {'A', 0}, {0xff, 0xfe, 0x00, 0xd8}} {
		if _, err := decodeText(bad); err == nil {
			t.Fatal("accepted invalid text")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Open(ctx, ".", Options{Path: p}); err == nil {
		t.Fatal("canceled preview succeeded")
	}
	if SafeText("safe\x1b]52;evil\a\n") != "safe]52;evil\n" {
		t.Fatal("unsafe terminal text")
	}
}

func TestInlineImageProtocolsAreEncodedAndRedirectedOutputNeverDetectsTTY(t *testing.T) {
	data := samplePNG(t)
	info, clean, err := imagePreview(data, 200)
	if err != nil || info.Width != 12 || info.Zoom != 200 {
		t.Fatalf("image: %+v %v", info, err)
	}
	for _, protocol := range []string{"kitty", "iterm2"} {
		encoded, err := EncodeImage(clean, protocol)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString(clean))) {
			t.Fatal("missing generated base64 payload")
		}
	}
	if _, err = EncodeImage([]byte("\x1b]52;bad"), "kitty"); err == nil {
		t.Fatal("text accepted as image")
	}
	env := func(k string) string {
		switch k {
		case "TERM":
			return "xterm-kitty"
		case "KITTY_WINDOW_ID":
			return "1"
		case "TERM_PROGRAM":
			return "iTerm.app"
		}
		return ""
	}
	if TerminalProtocol(&bytes.Buffer{}, env) != "none" {
		t.Fatal("redirected stream detected as terminal")
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if TerminalProtocol(null, env) != "none" {
		t.Fatal("character device is not an actual terminal")
	}
}

func iframeSource(t *testing.T, doc string) (string, string) {
	t.Helper()
	root, err := webhtml.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var source, sandbox string
	var walk func(*webhtml.Node)
	walk = func(n *webhtml.Node) {
		if n.Type == webhtml.ElementNode && n.Data == "iframe" {
			for _, a := range n.Attr {
				if a.Key == "srcdoc" {
					source = a.Val
				}
				if a.Key == "sandbox" {
					sandbox = a.Val
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return source, sandbox
}
func TestHTMLStaticAndInteractiveOpaqueSandbox(t *testing.T) {
	input := []byte(`<html><head><base href="file:///etc/"><meta http-equiv="refresh" content="0;url=https://evil.invalid"><script>parent.steal()</script><script src="https://evil.invalid/e.js"></script><link rel="stylesheet" href="/secret"></head><body onload="bad()"><a href="javascript:evil()">hello</a><iframe src="file:///etc/passwd"></iframe><img src="https://evil.invalid/i"><p style="color:red">visible</p></body></html>`)
	doc, err := HTMLDocument(input, false)
	if err != nil {
		t.Fatal(err)
	}
	source, sandbox := iframeSource(t, doc)
	for _, bad := range []string{"<script", "<base", "<iframe", "onload", "href=", "evil.invalid", "file:///etc"} {
		if strings.Contains(source, bad) {
			t.Fatalf("static source retained %s", bad)
		}
	}
	if sandbox != "" || !strings.Contains(source, "script-src &#39;none&#39;") && !strings.Contains(source, "script-src 'none'") {
		t.Fatalf("missing static sandbox: %q %s", sandbox, source)
	}
	doc, err = HTMLDocument(input, true)
	if err != nil {
		t.Fatal(err)
	}
	source, sandbox = iframeSource(t, doc)
	if sandbox != "allow-scripts" || !strings.Contains(source, "<script>parent.steal()</script>") || strings.Contains(source, "allow-same-origin") || strings.Contains(source, "evil.invalid") {
		t.Fatalf("interactive containment: %s %s", sandbox, source)
	}
}

func workbookZIP(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for name, data := range parts {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func fixtureParts() map[string]string {
	return map[string]string{
		"_rels/.rels":                `<Relationships><Relationship Id="root" Type="urn:rel/officeDocument" Target="Custom/book.xml"/></Relationships>`,
		"Custom/book.xml":            `<workbook xmlns:r="urn:rel"><sheets><sheet name="Data" sheetId="1" r:id="s1"/><sheet name="Hidden" state="hidden" r:id="s2"/></sheets></workbook>`,
		"Custom/_rels/book.xml.rels": `<Relationships><Relationship Id="s1" Type="urn:rel/worksheet" Target="../Sheets/data.xml"/><Relationship Id="s2" Type="urn:rel/worksheet" Target="/Sheets/hidden.xml"/><Relationship Id="ss" Type="urn:rel/sharedStrings" Target="../Shared/strings.xml"/><Relationship Id="style" Type="urn:rel/styles" Target="../Style/styles.xml"/></Relationships>`,
		"Shared/strings.xml":         `<sst><si><r><t>hello</t></r><r><t> world</t></r></si></sst>`,
		"Style/styles.xml":           `<styleSheet><numFmts><numFmt numFmtId="164" formatCode="0.00"/></numFmts><cellXfs><xf numFmtId="164" fontId="1" fillId="2" borderId="3"><alignment horizontal="center" wrapText="1"/></xf></cellXfs></styleSheet>`,
		"Sheets/data.xml":            `<worksheet><dimension ref="A1:C3"/><sheetViews><sheetView><pane state="frozen" xSplit="1" ySplit="1" topLeftCell="B2"/></sheetView></sheetViews><cols><col min="1" max="1" width="20" hidden="1"/></cols><sheetData><row r="1" ht="18" hidden="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><f>SUM(A2:A3)</f><v>3</v></c></row><row r="2"><c r="A2" t="inlineStr"><is><t>001</t></is></c><c r="C2"><f>1+1</f></c></row></sheetData><mergeCells><mergeCell ref="B2:C2"/></mergeCells><drawing/><conditionalFormatting/></worksheet>`,
		"Sheets/hidden.xml":          `<worksheet><sheetData/></worksheet>`,
	}
}
func TestSpreadsheetRelationshipsSavedFormulasMergesFrozenAndStyles(t *testing.T) {
	book, err := ReadWorkbook(context.Background(), workbookZIP(t, fixtureParts()), ".xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Sheets) != 2 || book.Sheets[1].State != "hidden" {
		t.Fatalf("sheets: %+v", book.Sheets)
	}
	s := book.Sheets[0]
	if s.Cells["A1"].Value != "hello world" || s.Cells["A2"].Value != "001" || s.Cells["B1"].Formula != "SUM(A2:A3)" || s.Cells["B1"].Value != "3" || s.Cells["C2"].Value != "" {
		t.Fatalf("cell values: %+v", s.Cells)
	}
	if s.Frozen == nil || s.Frozen.Rows != 1 || len(s.Merges) != 1 || len(s.HiddenRows) != 1 || s.RowHeights[1] != 18 || !s.ColumnWidths[0].Hidden {
		t.Fatalf("sheet geometry: %+v", s)
	}
	if len(book.Styles) != 1 || book.Styles[0].Font != 1 || book.NumberFormats[164] != "0.00" || len(book.Notices) < 3 {
		t.Fatalf("styles/notices: %+v", book)
	}
	if !strings.Contains(sheetText(s, 1, 1, 10), "B1 formula: =SUM(A2:A3)") {
		t.Fatal("formula bar equivalent missing")
	}
}

func TestSpreadsheetMalformedAndResourceLimits(t *testing.T) {
	parts := fixtureParts()
	parts["Sheets/data.xml"] = `<worksheet><dimension ref="A1:XFD1048576"/></worksheet>`
	if _, err := ReadWorkbook(context.Background(), workbookZIP(t, parts), ".xlsx"); err == nil {
		t.Fatal("huge rectangular sheet accepted")
	}
	parts = fixtureParts()
	parts["sheets/DATA.xml"] = `<worksheet/>`
	if _, err := ReadWorkbook(context.Background(), workbookZIP(t, parts), ".xlsx"); err == nil {
		t.Fatal("ambiguous case-equivalent parts accepted")
	}
	parts = fixtureParts()
	delete(parts, "Sheets/hidden.xml")
	if _, err := ReadWorkbook(context.Background(), workbookZIP(t, parts), ".xlsx"); err == nil {
		t.Fatal("missing relationship target accepted")
	}
	for _, bad := range []string{"\"unterminated,field", "a,\"bad\"tail\n"} {
		if _, err := ReadWorkbook(context.Background(), []byte(bad), ".csv"); err == nil {
			t.Fatal("malformed CSV accepted")
		}
	}
}

func TestCSVLiteralValuesQuotedMultilineAndUnevenRows(t *testing.T) {
	book, err := ReadWorkbook(context.Background(), []byte("001,=1+1,\"a,b\"\n\"two\nlines\",last\n"), ".csv")
	if err != nil {
		t.Fatal(err)
	}
	s := book.Sheets[0]
	if s.Rows != 2 || s.Columns != 3 || s.Cells["A1"].Value != "001" || s.Cells["B1"].Value != "=1+1" || s.Cells["B1"].Formula != "" || s.Cells["A2"].Value != "two\nlines" {
		t.Fatalf("literal CSV: %+v", s)
	}
}

func TestHTMLDirectAssetsBoundedAndEncodedWithoutMarkupInjection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "index.html")
	js := `document.querySelector('#value').textContent='local JS'; const marker='</script><script>bad()</script>';`
	css := `body {color:rgb(10,20,30)} /* </style><script>bad()</script> */`
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.css"), []byte(css), 0600); err != nil {
		t.Fatal(err)
	}
	data := []byte(`<link rel="stylesheet" href="app.css?version=1"><p id="value">original</p><script src="app.js"></script><script src="app.js"></script><script type="module" src="module.js"></script>`)
	bundled, assets, err := BundleHTML(context.Background(), source, data, true)
	if err != nil || len(assets) != 2 {
		t.Fatalf("assets: %v %v", assets, err)
	}
	if strings.Contains(string(bundled), "bad()") || !strings.Contains(string(bundled), base64.StdEncoding.EncodeToString([]byte(js))) || !strings.Contains(string(bundled), base64.StdEncoding.EncodeToString([]byte(css))) {
		t.Fatal("asset bytes were not safely encoded")
	}
	wrapped, err := HTMLDocument(bundled, true)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := iframeSource(t, wrapped)
	if !strings.Contains(inner, "data:text/javascript;base64,") || !strings.Contains(inner, "data:text/css;base64,") || strings.Contains(inner, "module.js") {
		t.Fatal("packed assets missing or module file loaded")
	}
	if _, _, err = BundleHTML(context.Background(), source, []byte(`<script src="missing.js"></script>`), true); err == nil {
		t.Fatal("missing direct local asset silently dropped")
	}
	static, assets, err := BundleHTML(context.Background(), source, data, false)
	if err != nil || len(assets) != 0 || !bytes.Equal(static, data) {
		t.Fatal("static preview loaded related files")
	}
}

func TestHTMLBrowserRenderingSmoke(t *testing.T) {
	if os.Getenv("DSGO_PREVIEW_RENDER_SMOKE") != "1" {
		t.Skip("opt-in installed-browser smoke")
	}
	if browserPath() == "" {
		t.Skip("no browser")
	}
	p := sourceFile(t, "hello.html", []byte(`<html><body style="background:white;font:32px sans-serif;color:#173042"><h1>dsgo preview</h1><p>Static HTML is rendered locally.</p><script>document.body.textContent='SCRIPT RAN'</script></body></html>`))
	r, err := Open(context.Background(), ".", Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if r.Image == nil || len(r.PNG) == 0 {
		t.Fatal("browser produced no image")
	}
	if output := os.Getenv("DSGO_PREVIEW_SMOKE_OUTPUT"); output != "" {
		if err = os.WriteFile(output, r.PNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
	svg := sourceFile(t, "shapes.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="600" height="300"><rect width="600" height="300" fill="#173042"/><circle cx="120" cy="150" r="80" fill="#59c3c3"/><text x="230" y="165" fill="white" font-family="sans-serif" font-size="40">dsgo SVG</text><script>document.documentElement.innerHTML='SCRIPT RAN'</script></svg>`))
	svgResult, err := Open(context.Background(), ".", Options{Path: svg})
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("DSGO_PREVIEW_SMOKE_OUTPUT"); output != "" {
		if err = os.WriteFile(strings.TrimSuffix(output, ".png")+"-svg.png", svgResult.PNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	interactive := filepath.Join(dir, "interactive.html")
	if err = os.WriteFile(interactive, []byte(`<link rel="stylesheet" href="app.css"><h1 id="value">Not loaded</h1><script src="app.js"></script>`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "app.css"), []byte(`body{font:32px sans-serif;background:#e8f4f5;color:#173042}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "app.js"), []byte(`document.querySelector('#value').textContent='Local JavaScript and CSS loaded';`), 0600); err != nil {
		t.Fatal(err)
	}
	interactiveResult, err := Open(context.Background(), ".", Options{Path: interactive, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("DSGO_PREVIEW_SMOKE_OUTPUT"); output != "" {
		if err = os.WriteFile(strings.TrimSuffix(output, ".png")+"-interactive.png", interactiveResult.PNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
