package preview

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	webhtml "golang.org/x/net/html"
)

// BundleHTML packs only direct classic .js and stylesheet .css references.
// Asset contents are base64 URLs so authored </script> or </style> text cannot
// break out into the generated document. Dynamic imports and CSS URLs remain
// browser-only resources and receive no filesystem access bridge.
func BundleHTML(ctx context.Context, source string, data []byte, interactive bool) ([]byte, []string, error) {
	if !interactive {
		return data, nil, nil
	}
	text, err := decodeText(data)
	if err != nil {
		return nil, nil, err
	}
	doc, err := webhtml.Parse(strings.NewReader(text))
	if err != nil {
		return nil, nil, err
	}
	assets := map[string][]byte{}
	ordered := []string{}
	total := 0
	hasBase := false
	var findBase func(*webhtml.Node)
	findBase = func(n *webhtml.Node) {
		if n.Type == webhtml.ElementNode && n.Data == "base" {
			for _, a := range n.Attr {
				if a.Key == "href" {
					hasBase = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findBase(c)
		}
	}
	findBase(doc)
	if hasBase {
		return data, nil, nil
	}
	load := func(reference, extension string) (string, error) {
		u, err := url.Parse(reference)
		if err != nil {
			return "", fmt.Errorf("invalid local HTML asset reference: %w", err)
		}
		if u.Scheme != "" || u.Host != "" || u.Path == "" || strings.HasPrefix(u.Path, "/") || !strings.EqualFold(filepath.Ext(u.Path), extension) {
			return "", nil
		}
		p, err := filepath.Abs(filepath.Join(filepath.Dir(source), filepath.FromSlash(u.Path)))
		if err != nil {
			return "", err
		}
		content, ok := assets[p]
		if !ok {
			if len(assets) >= 64 {
				return "", errors.New("HTML preview exceeds 64 direct local assets")
			}
			if err = ctx.Err(); err != nil {
				return "", err
			}
			f, err := os.Open(p)
			if err != nil {
				return "", fmt.Errorf("cannot load HTML asset %s: %w", p, err)
			}
			st, err := f.Stat()
			if err != nil {
				f.Close()
				return "", err
			}
			if !st.Mode().IsRegular() || st.Size() > 4<<20 {
				f.Close()
				return "", errors.New("HTML asset must be a regular file of at most 4 MiB")
			}
			content, err = io.ReadAll(io.LimitReader(f, 4<<20+1))
			closeErr := f.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
			if len(content) > 4<<20 || !utf8.Valid(content) {
				return "", errors.New("HTML asset exceeds 4 MiB or is not valid UTF-8")
			}
			if total+len(content) > 32<<20 {
				return "", errors.New("HTML direct assets exceed 32 MiB total")
			}
			total += len(content)
			assets[p] = content
			ordered = append(ordered, p)
		}
		media := "text/javascript"
		if extension == ".css" {
			media = "text/css"
		}
		return "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(content), nil
	}
	var visit func(*webhtml.Node) error
	visit = func(n *webhtml.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n.Type == webhtml.ElementNode {
			if n.Data == "script" {
				classic := true
				for _, a := range n.Attr {
					if a.Key == "type" && a.Val != "" && a.Val != "text/javascript" && a.Val != "application/javascript" {
						classic = false
					}
				}
				if classic {
					for i, a := range n.Attr {
						if a.Key == "src" {
							packed, err := load(a.Val, ".js")
							if err != nil {
								return err
							}
							if packed != "" {
								n.Attr[i].Val = packed
							}
						}
					}
				}
			}
			if n.Data == "link" {
				stylesheet := false
				for _, a := range n.Attr {
					if a.Key == "rel" && strings.EqualFold(strings.TrimSpace(a.Val), "stylesheet") {
						stylesheet = true
					}
				}
				if stylesheet {
					for i, a := range n.Attr {
						if a.Key == "href" {
							packed, err := load(a.Val, ".css")
							if err != nil {
								return err
							}
							if packed != "" {
								n.Attr[i].Val = packed
							}
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := visit(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err = visit(doc); err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	if err = webhtml.Render(&out, doc); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), ordered, nil
}
