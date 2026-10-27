package webtools

import (
	"bytes"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

const htmlOmitted = "[HTML content omitted: conversion could not be completed safely.]"

var voidElements = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true}
var inactiveElements = map[string]bool{"script": true, "style": true, "noscript": true, "iframe": true, "object": true, "embed": true, "template": true, "svg": true, "canvas": true, "head": true, "link": true, "meta": true}

func htmlMarkdown(source, base string) string {
	// The tokenizer pass bounds lexical depth before building the HTML tree.
	// Only a matching top-of-stack close reduces it; malformed input cannot
	// hide runaway nesting. Raw text and comments are handled by the tokenizer.
	tokens := html.NewTokenizer(strings.NewReader(source))
	stack := []string{}
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			if tokens.Err() != io.EOF {
				return htmlOmitted
			}
			break
		}
		tag, _ := tokens.TagName()
		name := strings.ToLower(string(tag))
		switch kind {
		case html.StartTagToken:
			if !voidElements[name] {
				stack = append(stack, name)
				if len(stack) > 512 {
					return htmlOmitted
				}
			}
		case html.EndTagToken:
			if len(stack) > 0 && stack[len(stack)-1] == name {
				stack = stack[:len(stack)-1]
			}
		}
	}
	root, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return htmlOmitted
	}
	baseURL, _ := url.Parse(base)
	nodes := 0
	var render func(*html.Node, int, bool) string
	render = func(node *html.Node, depth int, pre bool) string {
		nodes++
		if nodes > 200000 || depth > 512 {
			return htmlOmitted
		}
		if node.Type == html.CommentNode {
			return ""
		}
		if node.Type == html.TextNode {
			if pre {
				return node.Data
			}
			return collapseText(node.Data)
		}
		if node.Type == html.ElementNode && (inactiveElements[node.Data] || hiddenNode(node)) {
			return ""
		}
		if node.Type == html.ElementNode && node.Data == "table" {
			return renderTable(node, func(n *html.Node) string { return render(n, depth+1, false) })
		}
		var text strings.Builder
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			text.WriteString(render(child, depth+1, pre || node.Data == "pre"))
			if text.Len() > 1000000 {
				return htmlOmitted
			}
		}
		content := text.String()
		if node.Type != html.ElementNode {
			return content
		}
		switch node.Data {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			return "\n\n" + strings.Repeat("#", int(node.Data[1]-'0')) + " " + strings.TrimSpace(content) + "\n\n"
		case "p", "div", "section", "article", "header", "footer", "main", "aside", "ul", "ol", "dl":
			return "\n\n" + strings.TrimSpace(content) + "\n\n"
		case "br":
			return "\n"
		case "hr":
			return "\n\n---\n\n"
		case "li":
			return "\n- " + strings.TrimSpace(content) + "\n"
		case "strong", "b":
			return "**" + content + "**"
		case "em", "i":
			return "*" + content + "*"
		case "del", "s", "strike":
			return "~~" + content + "~~"
		case "code":
			if pre {
				return content
			}
			delimiter := "`"
			if strings.Contains(content, "`") {
				delimiter = "``"
			}
			return delimiter + content + delimiter
		case "pre":
			delimiter := "```"
			for strings.Contains(content, delimiter) {
				delimiter += "`"
			}
			return "\n\n" + delimiter + "\n" + strings.TrimRight(content, "\n") + "\n" + delimiter + "\n\n"
		case "blockquote":
			return "\n\n> " + strings.ReplaceAll(strings.TrimSpace(content), "\n", "\n> ") + "\n\n"
		case "a":
			if target := safeLink(attribute(node, "href"), baseURL); target != "" {
				return "[" + strings.TrimSpace(content) + "](" + target + ")"
			}
			return content
		case "img":
			alt := attribute(node, "alt")
			if target := safeLink(attribute(node, "src"), baseURL); target != "" {
				return "![" + alt + "](" + target + ")"
			}
			return alt
		}
		return content
	}
	text := render(root, 0, false)
	if strings.Contains(text, htmlOmitted) {
		return htmlOmitted
	}
	lines := strings.Split(text, "\n")
	var out bytes.Buffer
	blank := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimSpace(out.String())
}

func attribute(node *html.Node, name string) string {
	for _, a := range node.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
func hiddenNode(node *html.Node) bool {
	for _, a := range node.Attr {
		if a.Key == "hidden" || (a.Key == "aria-hidden" && strings.EqualFold(strings.TrimSpace(a.Val), "true")) {
			return true
		}
		if a.Key == "style" {
			style := strings.ToLower(strings.Join(strings.Fields(a.Val), ""))
			if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
				return true
			}
		}
	}
	return false
}

func collapseText(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		if s != "" {
			return " "
		}
		return ""
	}
	text := strings.Join(fields, " ")
	if strings.ContainsAny(s[:1], " \r\n\t") {
		text = " " + text
	}
	if strings.ContainsAny(s[len(s)-1:], " \r\n\t") {
		text += " "
	}
	return text
}

func safeLink(raw string, base *url.URL) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto") || u.User != nil {
		return ""
	}
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20", "\n", "%0A", "\r", "%0D").Replace(u.String())
}

func renderTable(table *html.Node, render func(*html.Node) string) string {
	rows := [][]string{}
	header := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (inactiveElements[n.Data] || hiddenNode(n)) {
			return
		}
		if n != table && n.Data == "table" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "tr" {
			row := []string{}
			for cell := n.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Type == html.ElementNode && (cell.Data == "th" || cell.Data == "td") {
					if len(rows) == 0 && cell.Data == "th" {
						header = true
					}
					text := render(cell)
					text = strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
					text = strings.ReplaceAll(text, "|", "\\|")
					row = append(row, text)
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(table)
	if len(rows) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n\n")
	for i, row := range rows {
		out.WriteString("| " + strings.Join(row, " | ") + " |\n")
		if i == 0 && header {
			for range row {
				out.WriteString("| --- ")
			}
			out.WriteString("|\n")
		}
	}
	return out.String() + "\n"
}
