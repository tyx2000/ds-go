// Package preview provides fixed local document viewers. It never loads plugins
// or sends the document to a model or remote rendering service.
package preview

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const MaxBytes = 64 << 20

type Options struct {
	Path        string `json:"path"`
	Renderer    string `json:"renderer,omitempty"`
	Line        int    `json:"line,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Search      string `json:"search,omitempty"`
	Page        int    `json:"page,omitempty"`
	Zoom        int    `json:"zoom,omitempty"`
	Sheet       string `json:"sheet,omitempty"`
	Row         int    `json:"row,omitempty"`
	Column      int    `json:"column,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
}
type Line struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}
type ImageInfo struct {
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Zoom   int    `json:"zoom"`
}
type Result struct {
	Path       string     `json:"path"`
	Kind       string     `json:"kind"`
	Renderer   string     `json:"renderer"`
	Lines      []Line     `json:"lines,omitempty"`
	TotalLines int        `json:"total_lines,omitempty"`
	NextLine   int        `json:"next_line,omitempty"`
	Matches    []int      `json:"matches,omitempty"`
	Text       string     `json:"text,omitempty"`
	Image      *ImageInfo `json:"image,omitempty"`
	PNG        []byte     `json:"-"`
	Workbook   *Workbook  `json:"workbook,omitempty"`
	SheetIndex int        `json:"sheet_index,omitempty"`
	Notices    []string   `json:"notices,omitempty"`
}

func normalize(o Options) (Options, error) {
	if strings.TrimSpace(o.Path) == "" {
		return o, errors.New("preview requires a file path")
	}
	if o.Line == 0 {
		o.Line = 1
	}
	if o.Limit == 0 {
		o.Limit = 100
	}
	if o.Page == 0 {
		o.Page = 1
	}
	if o.Zoom == 0 {
		o.Zoom = 100
	}
	if o.Row == 0 {
		o.Row = 1
	}
	if o.Column == 0 {
		o.Column = 1
	}
	if o.Line < 1 || o.Limit < 1 || o.Limit > 10000 || o.Page < 1 || o.Page > 100000 || o.Zoom < 25 || o.Zoom > 400 || o.Row < 1 || o.Column < 1 {
		return o, errors.New("invalid preview range: line/page/row/column >=1, limit1..10000, zoom25..400")
	}
	return o, nil
}

func readSource(ctx context.Context, cwd, path string) (string, []byte, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if !st.Mode().IsRegular() {
		return "", nil, errors.New("preview source is not a regular file")
	}
	if st.Size() > MaxBytes {
		return "", nil, errors.New("preview source exceeds 64 MiB")
	}
	var out bytes.Buffer
	buf := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return "", nil, err
		}
		n, e := f.Read(buf)
		if n > 0 {
			if out.Len()+n > MaxBytes {
				return "", nil, errors.New("preview source exceeds 64 MiB")
			}
			out.Write(buf[:n])
		}
		if e != nil {
			if !errors.Is(e, io.EOF) {
				return "", nil, e
			}
			break
		}
	}
	return path, out.Bytes(), nil
}

func Open(ctx context.Context, cwd string, opts Options) (Result, error) {
	o, err := normalize(opts)
	if err != nil {
		return Result{}, err
	}
	path, data, err := readSource(ctx, cwd, o.Path)
	if err != nil {
		return Result{}, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	renderer := o.Renderer
	if renderer == "" || renderer == "auto" {
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".svg":
			renderer = "image"
		case ".pdf":
			renderer = "pdf"
		case ".doc", ".docx", ".ppt", ".pptx":
			renderer = "office"
		case ".xlsx", ".xls", ".csv", ".tsv":
			renderer = "sheet"
		case ".html", ".htm":
			renderer = "html"
		case ".md", ".markdown":
			renderer = "markdown"
		default:
			renderer = "text"
		}
	}
	result := Result{Path: path, Kind: renderer, Renderer: renderer}
	switch renderer {
	case "text", "code", "markdown":
		text, err := decodeText(data)
		if err != nil {
			return Result{}, err
		}
		all := strings.Split(text, "\n")
		if len(all) > 0 && all[len(all)-1] == "" {
			all = all[:len(all)-1]
		}
		result.TotalLines = len(all)
		start := min(o.Line-1, len(all))
		end := min(start+o.Limit, len(all))
		result.Lines = []Line{}
		for i := start; i < end; i++ {
			result.Lines = append(result.Lines, Line{i + 1, all[i]})
		}
		if end < len(all) {
			result.NextLine = end + 1
		}
		if o.Search != "" {
			for i, line := range all {
				if strings.Contains(line, o.Search) {
					result.Matches = append(result.Matches, i+1)
					if len(result.Matches) == 10000 {
						result.Notices = append(result.Notices, "Search results capped at 10,000 source lines.")
						break
					}
				}
			}
		}
		if renderer == "markdown" {
			result.Notices = append(result.Notices, "Markdown source preview; graphical layout and inline images require a rich terminal viewer.")
		}
	case "image":
		if ext == ".svg" || ext == ".ico" {
			if ext == ".ico" && (len(data) < 6 || !bytes.Equal(data[:4], []byte{0, 0, 1, 0}) || data[4] == 0 && data[5] == 0) {
				return Result{}, errors.New("invalid ICO image header")
			}
			png, err := renderHTMLImage(ctx, data, true, o)
			if err != nil {
				return Result{}, err
			}
			data = png
			result.Notices = append(result.Notices, "Image rasterized by the local browser in a script-disabled image context; viewport fit may change intrinsic dimensions.")
		}
		if ext == ".gif" {
			result.Notices = append(result.Notices, "Animated GIF preview shows its first frame.")
		}
		result.Image, result.PNG, err = imagePreview(data, o.Zoom)
		if err != nil {
			return Result{}, err
		}
	case "pdf", "office":
		var pdf []byte
		pdf = data
		if renderer == "office" {
			pdf, err = convertOffice(ctx, path, data)
			if err != nil {
				return Result{}, err
			}
			result.Notices = append(result.Notices, "Converted with installed LibreOffice; exact official kit version and missing-font diagnostics are not available.")
		}
		png, text, err := renderPDF(ctx, pdf, o.Page, o.Zoom)
		if err != nil {
			return Result{}, err
		}
		result.Text = text
		result.Image, result.PNG, err = imagePreview(png, 100)
		if err != nil {
			return Result{}, err
		}
		result.Notices = append(result.Notices, fmt.Sprintf("Page %d at %d%%. Extracted text is separate from the terminal bitmap.", o.Page, o.Zoom))
	case "html":
		bundled, assets, err := BundleHTML(ctx, path, data, o.Interactive)
		if err != nil {
			return Result{}, err
		}
		png, err := renderHTMLImage(ctx, bundled, false, o)
		if err != nil {
			return Result{}, err
		}
		result.Image, result.PNG, err = imagePreview(png, 100)
		if err != nil {
			return Result{}, err
		}
		result.Notices = append(result.Notices, "Local browser viewport screenshot. Terminal bitmap does not provide browser interactions.")
		if len(assets) > 0 {
			result.Notices = append(result.Notices, fmt.Sprintf("Packed %d direct local CSS/classic JavaScript assets.", len(assets)))
		}
	case "sheet":
		book, err := ReadWorkbook(ctx, data, ext)
		if err != nil {
			return Result{}, err
		}
		result.Workbook = &book
		if o.Sheet != "" {
			index := -1
			for i, sheet := range book.Sheets {
				if sheet.Name == o.Sheet {
					index = i
					break
				}
			}
			if index < 0 {
				return Result{}, fmt.Errorf("worksheet %q not found", o.Sheet)
			}
			result.SheetIndex = index
		}
		result.Text = sheetText(book.Sheets[result.SheetIndex], o.Row, o.Column, o.Limit)
		result.Notices = append(result.Notices, book.Notices...)
	default:
		return Result{}, fmt.Errorf("unsupported preview renderer %q", renderer)
	}
	return result, nil
}

func decodeText(data []byte) (string, error) {
	if len(data) >= 2 && ((data[0] == 0xff && data[1] == 0xfe) || (data[0] == 0xfe && data[1] == 0xff)) {
		if (len(data)-2)%2 != 0 {
			return "", errors.New("invalid UTF-16 text; convert to UTF-8")
		}
		words := make([]uint16, (len(data)-2)/2)
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		for i := range words {
			words[i] = order.Uint16(data[2+i*2:])
		}
		for i, v := range words {
			if v >= 0xd800 && v <= 0xdbff {
				if i+1 >= len(words) || words[i+1] < 0xdc00 || words[i+1] > 0xdfff {
					return "", errors.New("invalid UTF-16 surrogate")
				}
			} else if v >= 0xdc00 && v <= 0xdfff && (i == 0 || words[i-1] < 0xd800 || words[i-1] > 0xdbff) {
				return "", errors.New("invalid UTF-16 surrogate")
			}
		}
		data = []byte(string(utf16.Decode(words)))
	} else {
		data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("file is not valid UTF-8/BOM-marked UTF-16 text; select a supported binary renderer or convert encoding")
	}
	return string(data), nil
}

// SafeText strips terminal control characters from all authored text. Image
// protocols are generated separately from validated bytes, never passed through.
func SafeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 32 && r != 127 && !(r >= 0x80 && r <= 0x9f) {
			return r
		}
		return -1
	}, s)
}
