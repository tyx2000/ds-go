package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"dsgo/internal/preview"
)

func parsePreviewOptions(arg string) (preview.Options, error) {
	arg = strings.TrimSpace(arg)
	var options preview.Options
	if arg == "" {
		return options, errors.New(`usage: /preview PATH or /preview {"path":"file","renderer":"text","line":1,"limit":100,"page":1,"zoom":100,"sheet":"Sheet1"}`)
	}
	data := ""
	if strings.HasPrefix(arg, "{") {
		data = arg
	} else if index := strings.Index(arg, " {"); index >= 0 {
		options.Path = strings.TrimSpace(arg[:index])
		data = arg[index+1:]
	} else {
		options.Path = arg
	}
	path := options.Path
	if data != "" {
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&options); err != nil {
			return options, err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return options, errors.New("preview options must be one JSON object")
		}
		if path != "" {
			if options.Path != "" && options.Path != path {
				return options, errors.New("preview path supplied twice")
			}
			options.Path = path
		}
	}
	if len(options.Path) >= 2 {
		if options.Path[0] == '"' && options.Path[len(options.Path)-1] == '"' || options.Path[0] == '\'' && options.Path[len(options.Path)-1] == '\'' {
			options.Path = options.Path[1 : len(options.Path)-1]
		}
	}
	return options, nil
}

func (a *App) previewDoctorInfo() map[string]any {
	info := preview.Doctor()
	info["detected_terminal_protocol"] = preview.TerminalProtocol(a.out, os.Getenv)
	return info
}

func (a *App) previewCommand(arg string) error {
	if strings.TrimSpace(arg) == "doctor" {
		a.print(a.previewDoctorInfo())
		return nil
	}
	options, err := parsePreviewOptions(arg)
	if err != nil {
		return err
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cwd := a.session.CWD
	if cwd == "" {
		cwd = a.flags.cwd
	}
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	result, err := preview.Open(ctx, cwd, options)
	if err != nil {
		return err
	}
	if a.flags.json {
		a.print(result)
		return nil
	}
	protocol := preview.TerminalProtocol(a.out, os.Getenv)
	if options.Protocol != "" && options.Protocol != "auto" {
		if options.Protocol == "none" {
			protocol = "none"
		} else if protocol != options.Protocol {
			return errors.New("requested inline image protocol was not detected on a real terminal")
		}
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	fmt.Fprintf(a.out, "%s [%s]\n", preview.SafeText(result.Path), preview.SafeText(result.Renderer))
	for _, notice := range result.Notices {
		fmt.Fprintf(a.out, "%s\n", preview.SafeText(notice))
	}
	if result.Image != nil {
		fmt.Fprintf(a.out, "%s %d×%d; zoom %d%%\n", result.Image.Format, result.Image.Width, result.Image.Height, result.Image.Zoom)
		if protocol == "none" {
			fmt.Fprintln(a.out, "Image bitmap ready; inline display requires a Kitty or iTerm2 terminal. No image control sequences were written.")
		} else {
			encoded, err := preview.EncodeImage(result.PNG, protocol)
			if err != nil {
				return err
			}
			if _, err = a.out.Write(encoded); err != nil {
				return err
			}
			fmt.Fprintln(a.out)
		}
	}
	for _, line := range result.Lines {
		fmt.Fprintf(a.out, "%6d  %s\n", line.Number, preview.SafeText(line.Text))
	}
	if result.NextLine > 0 {
		fmt.Fprintf(a.out, "Next source line: %d (total %d)\n", result.NextLine, result.TotalLines)
	}
	if len(result.Matches) > 0 {
		fmt.Fprintf(a.out, "Matching source lines: %v\n", result.Matches)
	}
	if result.Text != "" {
		fmt.Fprintln(a.out, preview.SafeText(result.Text))
	}
	if result.Workbook != nil {
		fmt.Fprint(a.out, "Worksheets:")
		for _, sheet := range result.Workbook.Sheets {
			fmt.Fprintf(a.out, " %s", preview.SafeText(sheet.Name))
		}
		fmt.Fprintln(a.out)
	}
	return nil
}
