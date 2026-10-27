package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dsgo/internal/tools"
	webhtml "golang.org/x/net/html"
)

func executable(names ...string) string {
	for _, name := range names {
		if filepath.IsAbs(name) {
			if st, err := os.Stat(name); err == nil && st.Mode().IsRegular() {
				return name
			}
		} else if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}
func browserPath() string {
	return executable("chromium", "chromium-browser", "google-chrome", "chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
}
func officePath() string {
	return executable("soffice", "libreoffice", "/Applications/LibreOffice.app/Contents/MacOS/soffice")
}
func Doctor() map[string]any {
	return map[string]any{"pdftoppm": executable("pdftoppm"), "pdftotext": executable("pdftotext"), "libreoffice": officePath(), "chromium": browserPath(), "inline_image_protocols": []string{"kitty", "iterm2"}, "native_image_formats": []string{"png", "jpeg", "gif (first frame)"}, "spreadsheets": []string{"xlsx", "csv", "tsv"}, "xls": "requires a legacy XLS parser; not available"}
}

type boundedLog struct{ bytes.Buffer }

func (b *boundedLog) Write(data []byte) (int, error) {
	n := len(data)
	if b.Len() < 65536 {
		b.Buffer.Write(data[:min(n, 65536-b.Len())])
	}
	return n, nil
}
func runRenderer(ctx context.Context, dir, exe string, args ...string) error {
	bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	mode := "workspace-write"
	if exe == browserPath() {
		// Chromium must install its own native child-process sandbox. macOS
		// refuses a second Seatbelt profile inside an already sandboxed process.
		// Keep Chromium's sandbox enabled; never pass --no-sandbox.
		mode = "full-access"
	}
	cmd, cleanup, err := tools.NewSandboxedCommand(bounded, dir, mode, exe, args...)
	if err != nil {
		return err
	}
	defer cleanup()
	// Chromium's Unix-domain singleton socket needs a short temporary path.
	// Its own profile and renderer writes remain inside this authorized directory.
	for i, value := range cmd.Env {
		if strings.HasPrefix(value, "TMPDIR=") {
			cmd.Env[i] = "TMPDIR=" + dir
		}
		if strings.HasPrefix(value, "TMP=") {
			cmd.Env[i] = "TMP=" + dir
		}
		if strings.HasPrefix(value, "TEMP=") {
			cmd.Env[i] = "TEMP=" + dir
		}
	}
	if runtime.GOOS == "darwin" {
		cmd.Env = append(cmd.Env, "MAC_CHROMIUM_TMPDIR="+dir)
	}
	var logs boundedLog
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err = cmd.Run(); err != nil {
		if bounded.Err() != nil {
			return bounded.Err()
		}
		return fmt.Errorf("local renderer failed: %s", SafeText(logs.String()))
	}
	return nil
}
func tempRenderer() (string, func(), error) {
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	dir, err := os.MkdirTemp(base, "dsgo-preview-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}
func outputBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > MaxBytes {
		return nil, errors.New("renderer output is not a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if len(b) > MaxBytes {
		return nil, errors.New("renderer output exceeds 64 MiB")
	}
	return b, err
}

func convertOffice(ctx context.Context, source string, data []byte) ([]byte, error) {
	exe := officePath()
	if exe == "" {
		return nil, errors.New("Office preview requires an installed LibreOffice renderer (soffice); no engine is installed or downloaded by dsgo")
	}
	dir, cleanup, err := tempRenderer()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	input := filepath.Join(dir, "source"+strings.ToLower(filepath.Ext(source)))
	if err = os.WriteFile(input, data, 0600); err != nil {
		return nil, err
	}
	profile := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "profile")}).String()
	profileUser := filepath.Join(dir, "profile", "user")
	if err = os.MkdirAll(profileUser, 0700); err != nil {
		return nil, err
	}
	security := `<?xml version="1.0"?><oor:items xmlns:oor="http://openoffice.org/2001/registry"><item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop></item></oor:items>`
	if err = os.WriteFile(filepath.Join(profileUser, "registrymodifications.xcu"), []byte(security), 0600); err != nil {
		return nil, err
	}
	err = runRenderer(ctx, dir, exe, "-env:UserInstallation="+profile, "--headless", "--nologo", "--nodefault", "--nofirststartwizard", "--convert-to", "pdf", "--outdir", dir, input)
	if err != nil {
		return nil, err
	}
	return outputBytes(filepath.Join(dir, "source.pdf"))
}
func renderPDF(ctx context.Context, data []byte, page, zoom int) ([]byte, string, error) {
	exe := executable("pdftoppm")
	if exe == "" {
		return nil, "", errors.New("PDF page rendering requires Poppler pdftoppm; optional selectable text extraction requires pdftotext")
	}
	dir, cleanup, err := tempRenderer()
	if err != nil {
		return nil, "", err
	}
	defer cleanup()
	input := filepath.Join(dir, "source.pdf")
	if err = os.WriteFile(input, data, 0600); err != nil {
		return nil, "", err
	}
	prefix := filepath.Join(dir, "page")
	p := strconv.Itoa(page)
	if err = runRenderer(ctx, dir, exe, "-f", p, "-l", p, "-singlefile", "-png", "-r", strconv.Itoa(96*zoom/100), "-scale-to", strconv.Itoa(1024*zoom/100), input, prefix); err != nil {
		return nil, "", err
	}
	png, err := outputBytes(prefix + ".png")
	if err != nil {
		return nil, "", err
	}
	text := ""
	if extractor := executable("pdftotext"); extractor != "" {
		out := filepath.Join(dir, "page.txt")
		if err = runRenderer(ctx, dir, extractor, "-f", p, "-l", p, "-layout", input, out); err != nil {
			return nil, "", err
		}
		raw, err := outputBytes(out)
		if err != nil {
			return nil, "", err
		}
		text = SafeText(string(raw))
	}
	return png, text, nil
}

// HTMLDocument creates an opaque-origin iframe. Static mode removes scripts,
// navigation, external resources and active containers. Interactive mode permits
// authored inline script inside the iframe; network and host-file access remain
// blocked. A screenshot cannot provide full interactive browser parity.
func HTMLDocument(data []byte, interactive bool) (string, error) {
	text, err := decodeText(data)
	if err != nil {
		return "", err
	}
	node, err := webhtml.Parse(strings.NewReader(text))
	if err != nil {
		return "", err
	}
	var sanitize func(*webhtml.Node)
	sanitize = func(n *webhtml.Node) {
		for child := n.FirstChild; child != nil; {
			next := child.NextSibling
			if child.Type == webhtml.ElementNode {
				tag := strings.ToLower(child.Data)
				packedCSS := false
				if interactive && tag == "link" {
					for _, a := range child.Attr {
						if a.Key == "href" && strings.HasPrefix(a.Val, "data:text/css;base64,") {
							packedCSS = true
						}
					}
				}
				drop := tag == "base" || tag == "iframe" || tag == "frame" || tag == "object" || tag == "embed" || tag == "form" || tag == "link" && !packedCSS || tag == "meta" || (!interactive && tag == "script")
				if tag == "script" {
					for _, a := range child.Attr {
						if strings.EqualFold(a.Key, "src") && !(interactive && strings.HasPrefix(a.Val, "data:text/javascript;base64,")) {
							drop = true
						}
					}
				}
				if drop {
					n.RemoveChild(child)
					child = next
					continue
				}
				attrs := child.Attr[:0]
				for _, a := range child.Attr {
					key := strings.ToLower(a.Key)
					if key == "href" && !packedCSS || key == "xlink:href" || key == "action" || key == "formaction" || key == "srcdoc" || key == "shadowrootmode" || key == "srcset" || strings.HasPrefix(key, "on") && !interactive {
						continue
					}
					if key == "src" && !(tag == "img" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), "data:image/")) && !(interactive && tag == "script" && strings.HasPrefix(a.Val, "data:text/javascript;base64,")) {
						continue
					}
					attrs = append(attrs, a)
				}
				child.Attr = attrs
			}
			sanitize(child)
			child = next
		}
	}
	sanitize(node)
	var source bytes.Buffer
	if err = webhtml.Render(&source, node); err != nil {
		return "", err
	}
	scripts := "'none'"
	sandbox := ""
	if interactive {
		scripts = "'unsafe-inline' data:"
		sandbox = "allow-scripts"
	}
	csp := "default-src 'none'; style-src 'unsafe-inline' data:; img-src data:; font-src data:; script-src " + scripts + "; connect-src 'none'; frame-src 'none'; object-src 'none'; form-action 'none'; base-uri 'none'"
	inner := "<!doctype html><meta http-equiv=\"Content-Security-Policy\" content=\"" + html.EscapeString(csp) + "\">" + source.String()
	return "<!doctype html><html><head><meta charset=utf-8><style>html,body,iframe{margin:0;padding:0;width:100%;height:100%;border:0}body{background:white}</style></head><body><iframe sandbox=\"" + sandbox + "\" srcdoc=\"" + html.EscapeString(inner) + "\"></iframe></body></html>", nil
}

func renderHTMLImage(ctx context.Context, data []byte, imageContext bool, o Options) ([]byte, error) {
	exe := browserPath()
	if exe == "" {
		return nil, errors.New("HTML/SVG image preview requires an installed headless Chromium browser; plain text is available with renderer:text")
	}
	var document string
	var err error
	if imageContext {
		media := http.DetectContentType(data)
		if bytes.Contains(bytes.ToLower(data[:min(512, len(data))]), []byte("<svg")) {
			media = "image/svg+xml"
		}
		document = "<!doctype html><meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; img-src data:; style-src 'unsafe-inline'\"><style>body{margin:0;background:white;height:100vh}img{max-width:100%;max-height:100vh;height:auto;object-fit:contain}</style><img src=\"data:" + media + ";base64," + base64.StdEncoding.EncodeToString(data) + "\">"
	} else {
		document, err = HTMLDocument(data, o.Interactive)
		if err != nil {
			return nil, err
		}
	}
	dir, cleanup, err := tempRenderer()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	input := filepath.Join(dir, "preview.html")
	output := filepath.Join(dir, "preview.png")
	if err = os.WriteFile(input, []byte(document), 0600); err != nil {
		return nil, err
	}
	fileURL := (&url.URL{Scheme: "file", Path: input}).String()
	err = runRenderer(ctx, dir, exe, "--headless=new", "--disable-gpu", "--disable-background-networking", "--disable-extensions", "--disable-crash-reporter", "--disable-breakpad", "--no-error-dialogs", "--no-first-run", "--no-default-browser-check", "--host-resolver-rules=MAP * ~NOTFOUND", "--user-data-dir="+filepath.Join(dir, "profile"), "--crash-dumps-dir="+filepath.Join(dir, "crashes"), "--hide-scrollbars", "--window-size=1200,900", "--timeout=10000", "--screenshot="+output, fileURL)
	if err != nil {
		return nil, err
	}
	return outputBytes(output)
}
