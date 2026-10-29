package context

import (
	"bytes"
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	textunicode "golang.org/x/text/encoding/unicode"
)

const DefaultInstructionMaxBytes = 64 << 10
const DefaultInstructionMaxSourceBytes = 1 << 20

type InstructionsConfig struct {
	HarnessHome, AgentsHome                         string
	ProjectRootMarkers, Candidates, LocalCandidates []string
	MaxBytes, MaxSourceBytes                        int
}

func DefaultInstructionConfig() InstructionsConfig {
	return InstructionsConfig{MaxBytes: DefaultInstructionMaxBytes, MaxSourceBytes: DefaultInstructionMaxSourceBytes}
}

type InstructionLoader struct{ config InstructionsConfig }
type InstructionSet struct {
	Rendered                    RenderedInstructions
	Observed, Deduped, Included []InstructionFile
	ProjectRoot, Identity       string
}
type instructionProbe struct {
	kind string
	file InstructionFile
	size int64
}

func NewInstructionLoader(config InstructionsConfig) (*InstructionLoader, error) {
	var err error
	config.HarnessHome, err = resolveInstructionHome(config.HarnessHome, "DSH_HOME", ".dsh")
	if err != nil {
		return nil, err
	}
	config.AgentsHome, err = resolveInstructionHome(config.AgentsHome, "DSH_AGENTS_HOME", ".agents")
	if err != nil {
		return nil, err
	}
	if config.ProjectRootMarkers == nil {
		config.ProjectRootMarkers = []string{".git"}
	} else {
		config.ProjectRootMarkers = append([]string{}, config.ProjectRootMarkers...)
	}
	config.Candidates = normalizeInstructionCandidates(config.Candidates, []string{"AGENTS.md", "CLAUDE.md"})
	config.LocalCandidates = normalizeInstructionCandidates(config.LocalCandidates, []string{"AGENTS.local.md", "CLAUDE.local.md"})
	return &InstructionLoader{config: config}, nil
}
func normalizeInstructionCandidates(values, fallback []string) []string {
	if values == nil {
		values = fallback
	}
	out := []string{}
	for _, value := range values {
		if value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\") {
			out = append(out, value)
		}
	}
	return out
}
func resolveInstructionHome(explicit, variable, defaultDir string) (string, error) {
	selected := explicit
	if selected == "" {
		if ambient, ok := os.LookupEnv(variable); ok && trimInstructionSpace(ambient) != "" {
			selected = ambient
		}
	}
	if selected == "" || selected == "~" || strings.HasPrefix(selected, "~/") || strings.HasPrefix(selected, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if selected == "" {
			selected = filepath.Join(home, defaultDir)
		} else if selected == "~" {
			selected = home
		} else {
			selected = filepath.Join(home, selected[2:])
		}
	}
	return filepath.Abs(selected)
}
func (loader *InstructionLoader) BaselineIdentity(cwd, root string) string {
	rel, _ := filepath.Rel(cwd, root)
	value := struct {
		ProjectRoot    string   `json:"projectRoot"`
		Markers        []string `json:"projectRootMarkers"`
		MaxBytes       int      `json:"maxBytes"`
		MaxSourceBytes int      `json:"maxSourceBytes"`
		Candidates     []string `json:"instructionFileCandidates"`
		Local          []string `json:"localInstructionFileCandidates"`
	}{rel, loader.config.ProjectRootMarkers, loader.config.MaxBytes, loader.config.MaxSourceBytes, loader.config.Candidates, loader.config.LocalCandidates}
	if rel == "." {
		value.ProjectRoot = ""
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(data.String(), "\n")
}
func (loader *InstructionLoader) FindProjectRoot(ctx stdctx.Context, cwd string) (string, error) {
	start, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	for current := start; ; current = filepath.Dir(current) {
		for _, marker := range loader.config.ProjectRootMarkers {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			_, err := os.Stat(filepath.Join(current, marker))
			if err == nil {
				return current, ctx.Err()
			}
			if !instructionPathMissing(err) {
				return "", err
			}
		}
		if current == filepath.Dir(current) {
			return start, ctx.Err()
		}
	}
}
func instructionPathMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || isInstructionNotDirectory(err)
}
func InstructionAncestorChain(root, cwd string) []string {
	root, _ = filepath.Abs(root)
	current, _ := filepath.Abs(cwd)
	chain := []string{}
	for current != root {
		chain = append(chain, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	chain = append(chain, root)
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}
func DescendantInstructionDirs(cwd, touched string) []string {
	cwd, _ = filepath.Abs(cwd)
	if !filepath.IsAbs(touched) {
		touched = filepath.Join(cwd, touched)
	}
	target := filepath.Dir(filepath.Clean(touched))
	rel, err := filepath.Rel(cwd, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil
	}
	return InstructionAncestorChain(cwd, target)[1:]
}
func relativeInstructionDisplay(root, path string) string {
	rel, _ := filepath.Rel(root, path)
	return EscapeProjectInstructionPath(rel)
}
func (loader *InstructionLoader) globalFiles() []InstructionFile {
	home, _ := os.UserHomeDir()
	defaultHarness, _ := filepath.Abs(filepath.Join(home, ".dsh"))
	defaultAgents, _ := filepath.Abs(filepath.Join(home, ".agents"))
	harnessLabel, agentsLabel := "$DSH_HOME", "$DSH_AGENTS_HOME"
	if loader.config.HarnessHome == defaultHarness {
		harnessLabel = "~/.dsh"
	}
	if loader.config.AgentsHome == defaultAgents {
		agentsLabel = "~/.agents"
	}
	return []InstructionFile{{AbsolutePath: filepath.Join(loader.config.HarnessHome, UserGlobalFile), DisplayPath: harnessLabel + "/" + UserGlobalFile}, {AbsolutePath: filepath.Join(loader.config.AgentsHome, UserGlobalFile), DisplayPath: agentsLabel + "/" + UserGlobalFile}}
}
func (loader *InstructionLoader) scopeFile(scope, root string) InstructionFile {
	directory, candidate := DecodeInstructionScope(scope)
	if IsUserGlobalDirectory(directory) {
		files := loader.globalFiles()
		if directory == UserGlobalDirectory {
			return files[0]
		}
		return files[1]
	}
	dir := root
	if directory != "." {
		dir = filepath.Join(root, directory)
	}
	absolute := filepath.Join(dir, candidate)
	return InstructionFile{AbsolutePath: absolute, DisplayPath: relativeInstructionDisplay(root, absolute)}
}
func instructionVersion(info os.FileInfo, path string) string {
	parts := []any{path, info.Mode().String(), info.Size(), info.ModTime().UnixNano()}
	if native := reflect.ValueOf(info.Sys()); native.IsValid() {
		if native.Kind() == reflect.Pointer {
			native = native.Elem()
		}
		if native.Kind() == reflect.Struct {
			for _, name := range []string{"Dev", "Ino", "Ctim", "Ctimespec", "CreationTime", "LastWriteTime"} {
				value := native.FieldByName(name)
				if value.IsValid() && value.CanInterface() {
					parts = append(parts, name, fmt.Sprint(value.Interface()))
				}
			}
		}
	}
	data, _ := json.Marshal(parts)
	return InstructionDigest(string(data))
}
func probeInstructionFile(ctx stdctx.Context, file InstructionFile) (instructionProbe, error) {
	if err := ctx.Err(); err != nil {
		return instructionProbe{}, err
	}
	info, err := os.Stat(file.AbsolutePath)
	if err := ctx.Err(); err != nil {
		return instructionProbe{}, err
	}
	if err != nil {
		if instructionPathMissing(err) {
			return instructionProbe{kind: "absent"}, nil
		}
		return instructionProbe{kind: "unavailable"}, nil
	}
	if !info.Mode().IsRegular() {
		return instructionProbe{kind: "absent"}, nil
	}
	file.Version = instructionVersion(info, file.AbsolutePath)
	return instructionProbe{kind: "present", file: file, size: info.Size()}, nil
}
func readInstructionFile(ctx stdctx.Context, probe instructionProbe, maxSource int) (*InstructionFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxSource <= 0 || probe.size > int64(maxSource) {
		return nil, nil
	}
	file, err := openInstructionFile(probe.file.AbsolutePath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	var data bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := file.Read(chunk)
		if data.Len()+n > maxSource {
			return nil, nil
		}
		data.Write(chunk[:n])
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, ctx.Err()
			}
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoded, err := textunicode.UTF8.NewDecoder().Bytes(data.Bytes())
	if err != nil || len(decoded) > maxSource {
		return nil, nil
	}
	loaded := probe.file
	loaded.Content = string(decoded)
	return &loaded, nil
}
func (loader *InstructionLoader) discover(ctx stdctx.Context, cwd, root string) ([]instructionProbe, error) {
	files := []InstructionFile{}
	files = append(files, loader.globalFiles()...)
	for _, dir := range InstructionAncestorChain(root, cwd) {
		for _, candidates := range [][]string{loader.config.Candidates, loader.config.LocalCandidates} {
			for _, candidate := range candidates {
				absolute := filepath.Join(dir, candidate)
				files = append(files, InstructionFile{AbsolutePath: absolute, DisplayPath: relativeInstructionDisplay(root, absolute)})
			}
		}
	}
	seen := map[string]bool{}
	out := []instructionProbe{}
	for _, file := range files {
		if seen[file.AbsolutePath] {
			continue
		}
		probe, err := probeInstructionFile(ctx, file)
		if err != nil {
			return nil, err
		}
		if probe.kind != "present" {
			continue
		}
		seen[file.AbsolutePath] = true
		out = append(out, probe)
	}
	return out, nil
}
func (loader *InstructionLoader) LoadBaseline(ctx stdctx.Context, cwd string, replace bool) (*InstructionSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if loader.config.MaxBytes <= 0 || loader.config.MaxSourceBytes <= 0 {
		return nil, nil
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	root, err := loader.FindProjectRoot(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return loader.loadBaselineAt(ctx, cwd, root, replace)
}
func (loader *InstructionLoader) loadBaselineAt(ctx stdctx.Context, cwd, root string, replace bool) (*InstructionSet, error) {
	probes, err := loader.discover(ctx, cwd, root)
	if err != nil {
		return nil, err
	}
	observed, kept, deduped := []InstructionFile{}, []InstructionFile{}, []InstructionFile{}
	groups := map[string]map[string]bool{}
	for _, probe := range probes {
		file, err := readInstructionFile(ctx, probe, loader.config.MaxSourceBytes)
		if err != nil {
			return nil, err
		}
		if file == nil {
			continue
		}
		observed = append(observed, *file)
		group := InstructionCandidateGroup(InstructionScopeDirectory(file.DisplayPath))
		if groups[group] == nil {
			groups[group] = map[string]bool{}
		}
		digest := TrimmedInstructionDigest(file.Content)
		if groups[group][digest] {
			deduped = append(deduped, *file)
			continue
		}
		groups[group][digest] = true
		kept = append(kept, *file)
	}
	if len(kept) == 0 && !replace {
		return nil, nil
	}
	rendered, included := RenderInstructionBaseline(kept, loader.config.MaxBytes, replace)
	return &InstructionSet{Rendered: rendered, Observed: observed, Deduped: deduped, Included: included, ProjectRoot: root, Identity: loader.BaselineIdentity(cwd, root)}, ctx.Err()
}
