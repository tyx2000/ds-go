package preview

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

const maxCells = 250000

type Cell struct {
	Address string `json:"address"`
	Value   string `json:"value"`
	Formula string `json:"formula,omitempty"`
	Style   int    `json:"style,omitempty"`
	Type    string `json:"type,omitempty"`
}
type Frozen struct {
	Rows    float64 `json:"rows"`
	Columns float64 `json:"columns"`
	TopLeft string  `json:"top_left,omitempty"`
}
type Sheet struct {
	Name         string          `json:"name"`
	State        string          `json:"state,omitempty"`
	Rows         int             `json:"rows"`
	Columns      int             `json:"columns"`
	Cells        map[string]Cell `json:"cells"`
	Merges       []string        `json:"merges,omitempty"`
	Frozen       *Frozen         `json:"frozen,omitempty"`
	RowHeights   map[int]float64 `json:"row_heights,omitempty"`
	HiddenRows   []int           `json:"hidden_rows,omitempty"`
	ColumnWidths []Column        `json:"column_widths,omitempty"`
}
type Column struct {
	Min    int     `xml:"min,attr" json:"min"`
	Max    int     `xml:"max,attr" json:"max"`
	Width  float64 `xml:"width,attr" json:"width"`
	Hidden bool    `xml:"hidden,attr" json:"hidden,omitempty"`
}
type Style struct {
	NumberFormat int `xml:"numFmtId,attr" json:"number_format_id"`
	Font         int `xml:"fontId,attr" json:"font_id"`
	Fill         int `xml:"fillId,attr" json:"fill_id"`
	Border       int `xml:"borderId,attr" json:"border_id"`
	Alignment    struct {
		Horizontal string `xml:"horizontal,attr" json:"horizontal,omitempty"`
		Vertical   string `xml:"vertical,attr" json:"vertical,omitempty"`
		Wrap       bool   `xml:"wrapText,attr" json:"wrap,omitempty"`
	} `xml:"alignment" json:"alignment"`
}
type Workbook struct {
	Sheets        []Sheet        `json:"sheets"`
	Styles        []Style        `json:"styles,omitempty"`
	NumberFormats map[int]string `json:"number_formats,omitempty"`
	Notices       []string       `json:"notices,omitempty"`
}
type richText string

func (r *richText) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var text strings.Builder
	depth := 1
	for depth > 0 {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch t := token.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				var part string
				if err = d.DecodeElement(&part, &t); err != nil {
					return err
				}
				text.WriteString(part)
			} else {
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	*r = richText(text.String())
	return nil
}

type relationship struct {
	ID     string `xml:"Id,attr"`
	Type   string `xml:"Type,attr"`
	Target string `xml:"Target,attr"`
	Mode   string `xml:"TargetMode,attr"`
}
type packageReader struct{ files map[string]*zip.File }

func archiveReader(data []byte) (packageReader, error) {
	if len(data) > 16<<20 {
		return packageReader{}, errors.New("spreadsheet source exceeds 16 MiB")
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return packageReader{}, err
	}
	p := packageReader{files: map[string]*zip.File{}}
	var total uint64
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.TrimPrefix(f.Name, "/")
		if path.Clean(name) != name || strings.Contains(name, "\\") || strings.HasPrefix(name, "../") {
			return p, errors.New("invalid spreadsheet ZIP part path")
		}
		key := strings.ToLower(name)
		if _, ok := p.files[key]; ok {
			return p, errors.New("ambiguous case-equivalent spreadsheet ZIP parts")
		}
		if f.UncompressedSize64 > 64<<20-total {
			return p, errors.New("spreadsheet archive exceeds 64 MiB decompressed")
		}
		total += f.UncompressedSize64
		p.files[key] = f
	}
	return p, nil
}
func (p packageReader) read(name string) ([]byte, error) {
	f := p.files[strings.ToLower(name)]
	if f == nil {
		return nil, fmt.Errorf("missing spreadsheet part %s", name)
	}
	if f.UncompressedSize64 > 16<<20 {
		return nil, errors.New("spreadsheet XML part exceeds 16 MiB")
	}
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, 16<<20+1))
	if len(b) > 16<<20 {
		return nil, errors.New("spreadsheet XML part exceeds 16 MiB")
	}
	return b, err
}
func (p packageReader) decode(name string, v any) error {
	b, err := p.read(name)
	if err != nil {
		return err
	}
	text, err := decodeText(b)
	if err != nil {
		return err
	}
	d := xml.NewDecoder(strings.NewReader(text))
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return d.Decode(v)
}
func resolvePart(base, target string) (string, error) {
	if strings.Contains(target, "\\") || strings.Contains(target, ":") {
		return "", errors.New("unsupported spreadsheet relationship target")
	}
	var p string
	if strings.HasPrefix(target, "/") {
		p = path.Clean(strings.TrimPrefix(target, "/"))
	} else {
		p = path.Clean(path.Join(path.Dir(base), target))
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", errors.New("spreadsheet relationship escapes package")
	}
	return p, nil
}
func (p packageReader) rels(part string) ([]relationship, error) {
	name := "_rels/.rels"
	if part != "" {
		name = path.Join(path.Dir(part), "_rels", path.Base(part)+".rels")
	}
	var x struct {
		R []relationship `xml:"Relationship"`
	}
	if err := p.decode(name, &x); err != nil {
		return nil, err
	}
	return x.R, nil
}

func ReadWorkbook(ctx context.Context, data []byte, ext string) (Workbook, error) {
	if len(data) > 16<<20 {
		return Workbook{}, errors.New("spreadsheet source exceeds 16 MiB")
	}
	if ext == ".csv" || ext == ".tsv" {
		return readDelimited(data, ext)
	}
	if ext == ".xls" {
		return Workbook{}, errors.New("legacy XLS preview requires a BIFF parser; this terminal build supports XLSX, CSV and TSV")
	}
	if ext != ".xlsx" {
		return Workbook{}, errors.New("unsupported spreadsheet format")
	}
	p, err := archiveReader(data)
	if err != nil {
		return Workbook{}, err
	}
	rels, err := p.rels("")
	if err != nil {
		return Workbook{}, err
	}
	workbookPath := ""
	for _, r := range rels {
		if strings.HasSuffix(r.Type, "/officeDocument") {
			if r.Mode == "External" {
				return Workbook{}, errors.New("external spreadsheet workbook relationship")
			}
			workbookPath, err = resolvePart("", r.Target)
			if err != nil {
				return Workbook{}, err
			}
			break
		}
	}
	if workbookPath == "" {
		return Workbook{}, errors.New("spreadsheet officeDocument relationship missing")
	}
	var root struct {
		Sheets []struct {
			Name  string `xml:"name,attr"`
			ID    string `xml:"id,attr"`
			State string `xml:"state,attr"`
		} `xml:"sheets>sheet"`
	}
	if err = p.decode(workbookPath, &root); err != nil {
		return Workbook{}, err
	}
	if len(root.Sheets) == 0 {
		return Workbook{}, errors.New("spreadsheet has no worksheets")
	}
	rels, err = p.rels(workbookPath)
	if err != nil {
		return Workbook{}, err
	}
	targets := map[string]relationship{}
	for _, r := range rels {
		if _, ok := targets[r.ID]; ok {
			return Workbook{}, errors.New("duplicate workbook relationship ID")
		}
		targets[r.ID] = r
	}
	book := Workbook{Sheets: []Sheet{}, NumberFormats: map[int]string{}}
	var shared []richText
	for _, r := range rels {
		if r.Mode == "External" {
			continue
		}
		part, err := resolvePart(workbookPath, r.Target)
		if err != nil {
			return Workbook{}, err
		}
		if strings.HasSuffix(r.Type, "/sharedStrings") {
			var x struct {
				Strings []richText `xml:"si"`
			}
			if err = p.decode(part, &x); err != nil {
				return Workbook{}, err
			}
			shared = x.Strings
		}
		if strings.HasSuffix(r.Type, "/styles") {
			var x struct {
				Styles  []Style `xml:"cellXfs>xf"`
				Formats []struct {
					ID   int    `xml:"numFmtId,attr"`
					Code string `xml:"formatCode,attr"`
				} `xml:"numFmts>numFmt"`
			}
			if err = p.decode(part, &x); err != nil {
				return Workbook{}, err
			}
			book.Styles = x.Styles
			for _, f := range x.Formats {
				book.NumberFormats[f.ID] = f.Code
			}
		}
	}
	area := 0
	missingFormula := false
	drawings := false
	for _, s := range root.Sheets {
		if err = ctx.Err(); err != nil {
			return Workbook{}, err
		}
		r, ok := targets[s.ID]
		if !ok || !strings.HasSuffix(r.Type, "/worksheet") || r.Mode == "External" {
			return Workbook{}, errors.New("worksheet relationship missing or unsupported")
		}
		part, err := resolvePart(workbookPath, r.Target)
		if err != nil {
			return Workbook{}, err
		}
		var source struct {
			Dimension struct {
				Ref string `xml:"ref,attr"`
			} `xml:"dimension"`
			Rows []struct {
				Number int     `xml:"r,attr"`
				Height float64 `xml:"ht,attr"`
				Hidden bool    `xml:"hidden,attr"`
				Cells  []struct {
					Address string   `xml:"r,attr"`
					Type    string   `xml:"t,attr"`
					Style   int      `xml:"s,attr"`
					Value   *string  `xml:"v"`
					Formula string   `xml:"f"`
					Inline  richText `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
			Merges []struct {
				Ref string `xml:"ref,attr"`
			} `xml:"mergeCells>mergeCell"`
			Panes []struct {
				State   string  `xml:"state,attr"`
				X       float64 `xml:"xSplit,attr"`
				Y       float64 `xml:"ySplit,attr"`
				TopLeft string  `xml:"topLeftCell,attr"`
			} `xml:"sheetViews>sheetView>pane"`
			Columns     []Column   `xml:"cols>col"`
			Drawings    []struct{} `xml:"drawing"`
			Conditional []struct{} `xml:"conditionalFormatting"`
		}
		if err = p.decode(part, &source); err != nil {
			return Workbook{}, err
		}
		sheet := Sheet{Name: s.Name, State: s.State, Cells: map[string]Cell{}, RowHeights: map[int]float64{}, ColumnWidths: source.Columns}
		if source.Dimension.Ref != "" {
			row, col, err := rangeExtent(source.Dimension.Ref)
			if err != nil {
				return Workbook{}, err
			}
			sheet.Rows, sheet.Columns = row, col
		}
		for _, row := range source.Rows {
			if row.Height > 0 {
				sheet.RowHeights[row.Number] = row.Height
			}
			if row.Hidden {
				sheet.HiddenRows = append(sheet.HiddenRows, row.Number)
			}
			lastColumn := 0
			for _, c := range row.Cells {
				address := c.Address
				if address == "" {
					address = columnName(lastColumn+1) + strconv.Itoa(row.Number)
				}
				rowNum, col, err := cellPosition(address)
				if err != nil {
					return Workbook{}, err
				}
				lastColumn = col
				sheet.Rows = max(sheet.Rows, rowNum)
				sheet.Columns = max(sheet.Columns, col)
				if int64(sheet.Rows)*int64(sheet.Columns) > maxCells {
					return Workbook{}, errors.New("spreadsheet exceeds 250,000 rectangular cells")
				}
				value := ""
				if c.Value != nil {
					value = *c.Value
				}
				switch c.Type {
				case "s":
					index, err := strconv.Atoi(value)
					if err != nil || index < 0 || index >= len(shared) {
						return Workbook{}, errors.New("invalid shared string index")
					}
					value = string(shared[index])
				case "inlineStr":
					value = string(c.Inline)
				}
				if c.Formula != "" && c.Value == nil {
					missingFormula = true
				}
				if _, ok := sheet.Cells[address]; ok {
					return Workbook{}, errors.New("duplicate worksheet cell")
				}
				sheet.Cells[address] = Cell{Address: address, Value: value, Formula: c.Formula, Style: c.Style, Type: c.Type}
			}
		}
		for _, m := range source.Merges {
			row, col, err := rangeExtent(m.Ref)
			if err != nil {
				return Workbook{}, err
			}
			sheet.Rows = max(sheet.Rows, row)
			sheet.Columns = max(sheet.Columns, col)
			sheet.Merges = append(sheet.Merges, m.Ref)
		}
		for _, pane := range source.Panes {
			if pane.State == "frozen" || pane.State == "frozenSplit" {
				sheet.Frozen = &Frozen{Rows: pane.Y, Columns: pane.X, TopLeft: pane.TopLeft}
				break
			}
		}
		if int64(sheet.Rows)*int64(sheet.Columns) > maxCells-int64(area) {
			return Workbook{}, errors.New("spreadsheet exceeds 250,000 combined rectangular cells")
		}
		area += sheet.Rows * sheet.Columns
		drawings = drawings || len(source.Drawings) > 0 || len(source.Conditional) > 0
		book.Sheets = append(book.Sheets, sheet)
	}
	book.Notices = append(book.Notices, "Saved cell values and formulas are shown without recalculation. Terminal table does not render fonts, fills, borders or number-format styling; style references and custom formats remain in JSON metadata.")
	if missingFormula {
		book.Notices = append(book.Notices, "Some formulas have no saved result; their displayed value is blank.")
	}
	if drawings {
		book.Notices = append(book.Notices, "Charts, drawings and conditional formatting are not displayed.")
	}
	return book, nil
}

func cellPosition(address string) (int, int, error) {
	if address == "" {
		return 0, 0, errors.New("empty spreadsheet cell address")
	}
	col, i := 0, 0
	for i < len(address) && address[i] >= 'A' && address[i] <= 'Z' {
		col = col*26 + int(address[i]-'A') + 1
		if col > 16384 {
			return 0, 0, errors.New("spreadsheet column outside supported range")
		}
		i++
	}
	row, err := strconv.Atoi(address[i:])
	if err != nil || row < 1 || row > 1048576 || col < 1 {
		return 0, 0, errors.New("invalid spreadsheet cell address")
	}
	return row, col, nil
}
func rangeExtent(ref string) (int, int, error) {
	parts := strings.Split(ref, ":")
	if len(parts) > 2 {
		return 0, 0, errors.New("invalid spreadsheet range")
	}
	row, col, err := cellPosition(parts[len(parts)-1])
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 2 {
		r, c, e := cellPosition(parts[0])
		if e != nil || r > row || c > col {
			return 0, 0, errors.New("invalid spreadsheet range")
		}
	}
	return row, col, nil
}
func columnName(n int) string {
	var b []byte
	for n > 0 {
		n--
		b = append([]byte{byte('A' + n%26)}, b...)
		n /= 26
	}
	return string(b)
}

func readDelimited(data []byte, ext string) (Workbook, error) {
	text, err := decodeText(data)
	if err != nil {
		return Workbook{}, err
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	if ext == ".tsv" {
		reader.Comma = '\t'
	}
	sheet := Sheet{Name: "Sheet1", Cells: map[string]Cell{}}
	for {
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Workbook{}, fmt.Errorf("malformed delimited spreadsheet: %w", err)
		}
		sheet.Rows++
		sheet.Columns = max(sheet.Columns, len(fields))
		if int64(sheet.Rows)*int64(sheet.Columns) > maxCells {
			return Workbook{}, errors.New("spreadsheet exceeds 250,000 rectangular cells")
		}
		for j, value := range fields {
			a := columnName(j+1) + strconv.Itoa(sheet.Rows)
			sheet.Cells[a] = Cell{Address: a, Value: value, Type: "literal"}
		}
	}
	return Workbook{Sheets: []Sheet{sheet}}, nil
}
func sheetText(sheet Sheet, row, column, limit int) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s (%d rows × %d columns)\n", SafeText(sheet.Name), sheet.Rows, sheet.Columns)
	if sheet.Frozen != nil {
		fmt.Fprintf(&out, "Frozen: %g rows, %g columns; origin %s\n", sheet.Frozen.Rows, sheet.Frozen.Columns, SafeText(sheet.Frozen.TopLeft))
	}
	if len(sheet.Merges) > 0 {
		fmt.Fprintf(&out, "Merged: %s\n", SafeText(strings.Join(sheet.Merges, ", ")))
	}
	endRow := min(sheet.Rows, row+limit-1)
	endColumn := min(sheet.Columns, column+19)
	for r := row; r <= endRow; r++ {
		for c := column; c <= endColumn; c++ {
			if c > column {
				out.WriteString("\t")
			}
			a := columnName(c) + strconv.Itoa(r)
			value := sheet.Cells[a].Value
			value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", "\\n"), "\t", "\\t")
			fmt.Fprintf(&out, "%s=%s", a, SafeText(value))
		}
		out.WriteString("\n")
	}
	addresses := make([]string, 0)
	for a, cell := range sheet.Cells {
		if cell.Formula != "" {
			r, c, _ := cellPosition(a)
			if r >= row && r <= endRow && c >= column && c <= endColumn {
				addresses = append(addresses, a)
			}
		}
	}
	sort.Strings(addresses)
	for _, a := range addresses {
		fmt.Fprintf(&out, "%s formula: =%s\n", a, SafeText(sheet.Cells[a].Formula))
	}
	return out.String()
}
