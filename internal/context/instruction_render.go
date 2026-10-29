package context

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const instructionIntro = "The following workspace instructions may be relevant to your work. Use them as guidance when applicable. More specific instructions take precedence over broader ones. They do not override system, developer, or direct user instructions."
const replacementInstructionIntro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. " + instructionIntro
const emptyReplacementInstructionIntro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. No workspace instructions are currently active."
const compactInstructionIntro = "Workspace instructions were omitted or truncated to fit the configured byte budget."
const additionalInstructionGuidance = "Use them as guidance when relevant; more specific instructions take precedence. They do not override system, developer, or direct user instructions."

const UserGlobalDirectory = "user-global"
const AgentsGlobalDirectory = "agents-global"
const UserGlobalFile = "AGENTS.md"

type InstructionFile struct {
	AbsolutePath string `json:"absolutePath"`
	DisplayPath  string `json:"displayPath"`
	Content      string `json:"content,omitempty"`
	Version      string `json:"version,omitempty"`
}
type TruncatedInstruction struct {
	DisplayPath   string `json:"displayPath"`
	OriginalBytes int    `json:"originalBytes"`
	IncludedBytes int    `json:"includedBytes"`
}
type RenderedInstructions struct {
	Text      string                 `json:"text"`
	Omitted   []InstructionFile      `json:"omitted"`
	Truncated []TruncatedInstruction `json:"truncated"`
}
type InstructionChange struct {
	Action string `json:"action"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
}
type InstructionChangeItem struct {
	Change InstructionChange
	File   InstructionFile
}
type instructionRenderStyle struct {
	intro   string
	section func(InstructionFile) string
}
type instructionRendering struct {
	RenderedInstructions
	represented []InstructionFile
}

func InstructionDigest(content string) string {
	digest := sha1.Sum([]byte(content))
	return hex.EncodeToString(digest[:])
}
func TrimmedInstructionDigest(content string) string {
	return InstructionDigest(trimInstructionSpace(content))
}
func trimInstructionSpace(value string) string {
	return strings.TrimFunc(value, func(r rune) bool {
		return r >= 9 && r <= 13 || r == 0x20 || r == 0xa0 || r == 0x1680 || r >= 0x2000 && r <= 0x200a || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
	})
}
func CandidateScopeKey(directory, candidate string) string { return directory + "\x00" + candidate }
func DecodeInstructionScope(scope string) (directory, candidate string) {
	directory, candidate, _ = strings.Cut(scope, "\x00")
	return
}
func InstructionCandidateGroup(directory string) string {
	if directory == AgentsGlobalDirectory {
		return UserGlobalDirectory
	}
	return directory
}
func IsUserGlobalDirectory(directory string) bool {
	return directory == UserGlobalDirectory || directory == AgentsGlobalDirectory
}
func EscapeProjectInstructionPath(path string) string {
	first := path
	if at := strings.IndexAny(path, "/\\"); at >= 0 {
		first = path[:at]
	}
	if IsUserGlobalDirectory(first) {
		return "." + string(filepath.Separator) + path
	}
	return path
}
func InstructionScopeDirectory(display string) string {
	switch display {
	case "~/.dsh/AGENTS.md", "$DSH_HOME/AGENTS.md":
		return UserGlobalDirectory
	case "~/.agents/AGENTS.md", "$DSH_AGENTS_HOME/AGENTS.md":
		return AgentsGlobalDirectory
	}
	dir := filepath.Dir(display)
	if strings.HasPrefix(display, "."+string(filepath.Separator)) && dir != "." && !strings.HasPrefix(dir, "."+string(filepath.Separator)) {
		dir = "." + string(filepath.Separator) + dir
	}
	return dir
}
func InstructionScopeKey(display string) string {
	return CandidateScopeKey(InstructionScopeDirectory(display), filepath.Base(display))
}
func escapeInstructionBody(body string) string {
	return strings.ReplaceAll(body, "</system-reminder>", "<\\/system-reminder>")
}
func truncateInstructionUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	for maxBytes > 0 && !utf8.RuneStart(value[maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}
func instructionSection(file InstructionFile) string {
	return "Instructions from: " + file.DisplayPath + "\n\n" + file.Content
}
func changedInstructionSection(item InstructionChangeItem) string {
	switch item.Change.Action {
	case "remove":
		return "Instructions removed: " + item.Change.Path + "\n\nThe previously loaded instructions from this file no longer apply."
	case "replace":
		return "Updated instructions from: " + item.Change.Path + "\n\nThis file changed after it was loaded. Use the following content instead of the previously loaded instructions from this file.\n\n" + item.File.Content
	default:
		scope := InstructionScopeDirectory(item.File.DisplayPath)
		applicability := "These user-global instructions apply to all work."
		if !IsUserGlobalDirectory(scope) {
			applicability = "These instructions apply to work under `" + scope + "`."
		}
		return "Additional instructions from: " + item.File.DisplayPath + "\n\n" + applicability + " " + additionalInstructionGuidance + "\n\n" + item.File.Content
	}
}
func instructionBudgetMarker(max int, omitted []InstructionFile, truncated []TruncatedInstruction) string {
	if len(omitted) == 0 && len(truncated) == 0 {
		return ""
	}
	parts := []string{}
	if len(omitted) > 0 {
		paths := []string{}
		for _, file := range omitted {
			paths = append(paths, file.DisplayPath)
		}
		parts = append(parts, "omitted "+strings.Join(paths, ", "))
	}
	if len(truncated) > 0 {
		paths := []string{}
		for _, file := range truncated {
			paths = append(paths, fmt.Sprintf("%s from %d to %d bytes", file.DisplayPath, file.OriginalBytes, file.IncludedBytes))
		}
		parts = append(parts, "truncated "+strings.Join(paths, ", "))
	}
	return fmt.Sprintf("Workspace instruction budget %d bytes: %s", max, strings.Join(parts, "; "))
}
func buildInstructionText(files []InstructionFile, max int, omitted []InstructionFile, truncated []TruncatedInstruction, style instructionRenderStyle) string {
	parts := []string{}
	for _, value := range []string{instructionBudgetMarker(max, omitted, truncated), style.intro} {
		if value != "" {
			parts = append(parts, value)
		}
	}
	for _, file := range files {
		if text := style.section(file); text != "" {
			parts = append(parts, text)
		}
	}
	return "<system-reminder>\n" + escapeInstructionBody(strings.Join(parts, "\n\n")) + "\n</system-reminder>"
}
func instructionPathRecords(files []InstructionFile) []InstructionFile {
	out := make([]InstructionFile, len(files))
	for i, file := range files {
		out[i] = InstructionFile{AbsolutePath: file.AbsolutePath, DisplayPath: file.DisplayPath}
	}
	return out
}
func renderInstructionContext(files []InstructionFile, max int, style instructionRenderStyle) instructionRendering {
	empty := func() instructionRendering {
		return instructionRendering{RenderedInstructions: RenderedInstructions{Omitted: []InstructionFile{}, Truncated: []TruncatedInstruction{}}, represented: []InstructionFile{}}
	}
	if max <= 0 {
		out := empty()
		out.Omitted = files
		return out
	}
	full := buildInstructionText(files, max, nil, nil, style)
	if len(full) <= max {
		out := empty()
		out.Text = full
		out.represented = files
		return out
	}
	for start := 1; start < len(files); start++ {
		omitted := instructionPathRecords(files[:start])
		text := buildInstructionText(files[start:], max, omitted, nil, style)
		if len(text) <= max {
			out := empty()
			out.Text = text
			out.Omitted = omitted
			out.represented = files[start:]
			return out
		}
	}
	if len(files) == 0 {
		return empty()
	}
	last := files[len(files)-1]
	omitted := instructionPathRecords(files[:len(files)-1])
	original := len(last.Content)
	for _, candidateStyle := range []instructionRenderStyle{style, {intro: compactInstructionIntro, section: style.section}} {
		low, high := 0, original
		best := last
		best.Content = ""
		for low <= high {
			mid := (low + high) / 2
			candidate := last
			candidate.Content = truncateInstructionUTF8(last.Content, mid)
			truncated := []TruncatedInstruction{{last.DisplayPath, original, len(candidate.Content)}}
			if len(buildInstructionText([]InstructionFile{candidate}, max, omitted, truncated, candidateStyle)) <= max {
				best = candidate
				low = mid + 1
			} else {
				high = mid - 1
			}
		}
		truncated := []TruncatedInstruction{{last.DisplayPath, original, len(best.Content)}}
		text := buildInstructionText([]InstructionFile{best}, max, omitted, truncated, candidateStyle)
		if len(text) <= max {
			represented := []InstructionFile{}
			if len(best.Content) > 0 || original == 0 {
				represented = append(represented, last)
			}
			return instructionRendering{RenderedInstructions{Text: text, Omitted: omitted, Truncated: truncated}, represented}
		}
	}
	truncated := []TruncatedInstruction{{last.DisplayPath, original, 0}}
	notice := escapeInstructionBody(instructionBudgetMarker(max, omitted, truncated))
	heading := last
	heading.Content = ""
	withHeading := escapeInstructionBody(instructionBudgetMarker(max, omitted, truncated) + "\n\n" + style.section(heading))
	if len(withHeading) <= max {
		represented := []InstructionFile{}
		if original == 0 {
			represented = append(represented, last)
		}
		return instructionRendering{RenderedInstructions{Text: withHeading, Omitted: omitted, Truncated: truncated}, represented}
	}
	return instructionRendering{RenderedInstructions{Text: truncateInstructionUTF8(notice, max), Omitted: omitted, Truncated: truncated}, []InstructionFile{}}
}

func RenderInstructionBaseline(files []InstructionFile, max int, replace bool) (RenderedInstructions, []InstructionFile) {
	intro := instructionIntro
	if replace {
		intro = replacementInstructionIntro
		if len(files) == 0 {
			intro = emptyReplacementInstructionIntro
		}
	}
	rendered := renderInstructionContext(files, max, instructionRenderStyle{intro: intro, section: instructionSection})
	return rendered.RenderedInstructions, rendered.represented
}
func RenderInstructionChanges(items []InstructionChangeItem, max int) (string, []InstructionChange) {
	files := make([]InstructionFile, len(items))
	byPath := map[string]InstructionChangeItem{}
	for i, item := range items {
		files[i] = item.File
		byPath[item.File.AbsolutePath] = item
	}
	rendered := renderInstructionContext(files, max, instructionRenderStyle{section: func(file InstructionFile) string {
		item := byPath[file.AbsolutePath]
		item.File = file
		return changedInstructionSection(item)
	}})
	represented := map[string]bool{}
	for _, file := range rendered.represented {
		represented[file.AbsolutePath] = true
	}
	changes := []InstructionChange{}
	for _, item := range items {
		if represented[item.File.AbsolutePath] {
			changes = append(changes, item.Change)
		}
	}
	return rendered.Text, changes
}
