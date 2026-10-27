package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

type PagerOptions struct {
	Input    *os.File
	Output   io.Writer
	Terminal *os.File
	Load     func() (Transcript, error)
	Turn     int
}
type pager struct {
	options                        PagerOptions
	data                           Transcript
	turn, selected, top, detailTop int
	detail, stats, outline, follow bool
	tab                            string
	search                         string
	searching                      bool
	key                            []byte
	status                         string
	width, height                  int
}

// Page takes terminal ownership only for its bounded foreground interaction.
// It always restores terminal attributes, cursor and primary screen, including cancellation.
func Page(ctx context.Context, o PagerOptions) error {
	if o.Input == nil || o.Output == nil || o.Load == nil {
		return fmt.Errorf("trajectory pager requires input, output and data")
	}
	restore, err := rawTerminal(o.Input)
	if err != nil {
		return err
	}
	defer restore()
	p := pager{options: o, turn: o.Turn, follow: true, tab: "summary", width: 80, height: 24}
	if err = p.refresh(); err != nil {
		return err
	}
	if _, err = io.WriteString(o.Output, "\x1b[?1049h\x1b[?25l"); err != nil {
		return err
	}
	defer func() { _, _ = io.WriteString(o.Output, "\x1b[?25h\x1b[?1049l") }()
	nextLoad := time.Now().Add(500 * time.Millisecond)
	dirty := true
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		w, h := terminalSize(o.Terminal)
		if w != p.width || h != p.height {
			p.width, p.height = w, h
			dirty = true
		}
		if !now.Before(nextLoad) {
			if err = p.refresh(); err != nil {
				p.status = err.Error()
			}
			nextLoad = now.Add(500 * time.Millisecond)
			dirty = true
		}
		if dirty {
			if err = p.draw(); err != nil {
				return err
			}
			dirty = false
		}
		var b [32]byte
		n, readErr := o.Input.Read(b[:])
		if n > 0 {
			for _, key := range p.decodeKeys(b[:n]) {
				if p.handle(key) {
					return nil
				}
				dirty = true
			}
		}
		if n == 0 && len(p.key) == 1 && p.key[0] == 0x1b {
			p.key = nil
			p.handle("escape")
			dirty = true
		}
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
	}
}
func (p *pager) refresh() error {
	data, err := p.options.Load()
	if err != nil {
		return err
	}
	p.data = data
	rows := p.rows()
	if p.follow {
		p.selected = max(0, len(rows)-1)
	} else {
		p.selected = min(p.selected, max(0, len(rows)-1))
	}
	return nil
}
func (p *pager) rows() []Record {
	out := []Record{}
	for _, r := range p.data.Records {
		if p.turn == 0 || r.Turn == p.turn {
			out = append(out, r)
		}
	}
	return out
}
func (p *pager) draw() error {
	width, height := max(20, p.width), max(5, p.height)
	bodyHeight := height - 3
	var body []string
	title := "Trajectory · all turns"
	if p.turn > 0 {
		title = fmt.Sprintf("Trajectory · turn %d", p.turn)
	}
	rows := p.rows()
	switch {
	case p.stats:
		body = Wrap(Statistics(p.data.Stats), width)
		title = "Trajectory · session statistics"
	case p.outline:
		body = Wrap(Outline(p.data), width)
		title = "Trajectory · turn outline"
	case p.detail && len(rows) > 0:
		r := rows[min(p.selected, len(rows)-1)]
		body = Wrap(Inspector(r, p.tab), width)
		title = fmt.Sprintf("Inspect · %s · %s · %s", r.Kind, r.Name, p.tab)
	default:
		for i, r := range rows {
			marker := "  "
			if i == p.selected {
				marker = "> "
			}
			body = append(body, clipCells(marker+RecordLine(r), width))
		}
		if len(body) == 0 {
			body = []string{"No recorded activity."}
		}
	}
	top := p.top
	if p.detail || p.stats || p.outline {
		top = p.detailTop
	} else {
		if p.selected < top {
			top = p.selected
		}
		if p.selected >= top+bodyHeight {
			top = p.selected - bodyHeight + 1
		}
		p.top = top
	}
	top = min(max(0, top), max(0, len(body)-bodyHeight))
	if p.detail || p.stats || p.outline {
		p.detailTop = top
	}
	var frame strings.Builder
	frame.WriteString("\x1b[H\x1b[2J")
	fmt.Fprint(&frame, clipCells(Safe(title), width), "\x1b[K\r\n")
	for row := 0; row < bodyHeight; row++ {
		line := ""
		if top+row < len(body) {
			line = body[top+row]
		}
		frame.WriteString(line)
		frame.WriteString("\x1b[K\r\n")
	}
	help := "j/k ↑↓ · PgUp/PgDn · Enter inspect · [/] turn · s stats · u outline · / search · q back"
	if p.detail {
		help = "j/k scroll · Tab view · i input · o output · r raw · c code · t timing · Enter back · q return"
	}
	if p.searching {
		help = "Search: " + p.search
	} else if p.search != "" {
		help = "Search: " + p.search + " · n/N next/previous · " + help
	}
	frame.WriteString(clipCells(help, width))
	frame.WriteString("\x1b[K\r\n")
	footer := p.status
	if footer == "" {
		footer = fmt.Sprintf("%d/%d records · %d turns · follow %v", min(len(rows), p.selected+1), len(rows), len(p.data.Turns), p.follow)
	}
	frame.WriteString(clipCells(Safe(footer), width))
	frame.WriteString("\x1b[K")
	_, err := io.WriteString(p.options.Output, frame.String())
	return err
}
func (p *pager) handle(key string) bool {
	if p.searching {
		switch key {
		case "enter":
			p.searching = false
			p.find(1)
		case "escape":
			p.searching = false
		case "backspace":
			r := []rune(p.search)
			if len(r) > 0 {
				p.search = string(r[:len(r)-1])
			}
		default:
			if len(key) > 0 && !strings.Contains(key, "\x1b") && utf8.RuneCountInString(key) == 1 {
				p.search += key
			}
		}
		return false
	}
	page := max(1, p.height-4)
	rows := p.rows()
	scroll := func(n int) {
		p.follow = false
		if p.detail || p.stats || p.outline {
			p.detailTop = max(0, p.detailTop+n)
		} else {
			p.selected = min(max(0, p.selected+n), max(0, len(rows)-1))
		}
	}
	switch key {
	case "q", "ctrl-c":
		return true
	case "j", "down":
		scroll(1)
	case "k", "up":
		scroll(-1)
	case "page-down", "ctrl-f", " ":
		scroll(page)
	case "page-up", "ctrl-b":
		scroll(-page)
	case "home", "g":
		p.follow = false
		p.selected = 0
		p.top = 0
		p.detailTop = 0
	case "end", "G":
		p.follow = true
		p.selected = max(0, len(rows)-1)
		p.detailTop = 1 << 30
	case "enter":
		p.detail = !p.detail
		p.stats = false
		p.outline = false
		p.detailTop = 0
	case "escape":
		p.detail = false
		p.stats = false
		p.outline = false
		p.detailTop = 0
	case "s":
		p.stats = !p.stats
		p.detail = false
		p.outline = false
		p.detailTop = 0
	case "u":
		p.outline = !p.outline
		p.stats = false
		p.detail = false
		p.detailTop = 0
	case "[":
		if p.turn == 0 {
			p.turn = len(p.data.Turns)
		} else {
			p.turn = max(1, p.turn-1)
		}
		p.selected = 0
		p.top = 0
		p.detailTop = 0
		p.follow = false
	case "]":
		p.turn = min(len(p.data.Turns), p.turn+1)
		p.selected = 0
		p.top = 0
		p.detailTop = 0
		p.follow = false
	case "0":
		p.turn = 0
		p.selected = 0
		p.top = 0
		p.follow = false
	case "/":
		p.searching = true
		p.search = ""
	case "n":
		p.find(1)
	case "N":
		p.find(-1)
	case "tab":
		if p.detail {
			tabs := []string{"summary", "input", "output", "timing", "raw", "code"}
			for i, t := range tabs {
				if t == p.tab {
					p.tab = tabs[(i+1)%len(tabs)]
					break
				}
			}
			p.detailTop = 0
		}
	case "i", "o", "r", "c", "t":
		if p.detail {
			p.tab = map[string]string{"i": "input", "o": "output", "r": "raw", "c": "code", "t": "timing"}[key]
			p.detailTop = 0
		}
	}
	return false
}
func (p *pager) find(direction int) {
	if p.search == "" {
		return
	}
	rows := p.rows()
	query := strings.ToLower(p.search)
	for delta := 1; delta <= len(rows); delta++ {
		i := (p.selected + direction*delta) % len(rows)
		if i < 0 {
			i += len(rows)
		}
		r := rows[i]
		hay := strings.ToLower(r.Name + "\n" + r.Text + "\n" + r.Error + "\n" + string(r.Input) + "\n" + string(r.Output))
		if strings.Contains(hay, query) {
			p.selected = i
			p.follow = false
			p.status = ""
			return
		}
	}
	p.status = "No matching record"
}
func (p *pager) decodeKeys(input []byte) []string {
	p.key = append(p.key, input...)
	out := []string{}
	for len(p.key) > 0 {
		b := p.key[0]
		if b == 0x1b {
			if len(p.key) == 1 {
				break
			}
			if p.key[1] == '[' {
				end := 2
				for end < len(p.key) && (p.key[end] < 0x40 || p.key[end] > 0x7e) {
					end++
				}
				if end >= len(p.key) {
					break
				}
				seq := string(p.key[2 : end+1])
				key := map[string]string{"A": "up", "B": "down", "5~": "page-up", "6~": "page-down", "H": "home", "F": "end", "1~": "home", "4~": "end"}[seq]
				if key != "" {
					out = append(out, key)
				}
				p.key = p.key[end+1:]
				continue
			}
			out = append(out, "escape")
			p.key = p.key[1:]
			continue
		}
		switch b {
		case 3:
			out = append(out, "ctrl-c")
		case 2:
			out = append(out, "ctrl-b")
		case 6:
			out = append(out, "ctrl-f")
		case '\r', '\n':
			out = append(out, "enter")
		case '\t':
			out = append(out, "tab")
		case 8, 127:
			out = append(out, "backspace")
		default:
			if !utf8.FullRune(p.key) {
				return out
			}
			r, n := utf8.DecodeRune(p.key)
			if r >= 32 {
				out = append(out, string(r))
			}
			p.key = p.key[n:]
			continue
		}
		p.key = p.key[1:]
	}
	return out
}
func clipCells(s string, width int) string {
	var out strings.Builder
	cells := 0
	for _, r := range Safe(s) {
		if r == '\n' || r == '\t' {
			r = ' '
		}
		w := runeWidth(r)
		if cells+w > width {
			break
		}
		out.WriteRune(r)
		cells += w
	}
	return out.String()
}
