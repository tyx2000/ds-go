package cli

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"dsgo/internal/core"
	"dsgo/internal/model"
	"dsgo/internal/reference"
	"dsgo/internal/tui"
)

func (a *App) closeFileSearches() {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	for id, search := range a.fileSearches {
		_ = search.index.Close()
		delete(a.fileSearches, id)
	}
}
func (a *App) invalidateFileIndex(id string) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()
	if search, ok := a.fileSearches[id]; ok {
		search.index.Invalidate()
	}
}
func (a *App) fileCandidates(ctx context.Context, id, query string) ([]reference.FileCandidate, error) {
	cwd, err := a.ensureWorkingDirectory(ctx, id)
	if err != nil {
		return nil, err
	}
	a.fileMu.Lock()
	search, ok := a.fileSearches[id]
	if !ok || search.cwd != cwd {
		if ok {
			_ = search.index.Close()
		}
		options := reference.FileOptions{}
		if cfg := a.cfg.FileReferences; cfg != nil {
			if cfg.MaxResults != nil {
				options.MaxResults = *cfg.MaxResults
			}
			if cfg.MaxEntries != nil {
				options.MaxEntries = *cfg.MaxEntries
			}
			if cfg.ExcludedDirectories != nil {
				options.ExcludedDirectories = append([]string{}, (*cfg.ExcludedDirectories)...)
			}
		}
		index, err := reference.NewFiles(cwd, options)
		if err != nil {
			a.fileMu.Unlock()
			return nil, err
		}
		search = fileSearch{cwd: cwd, index: index}
		if a.fileSearches == nil {
			a.fileSearches = map[string]fileSearch{}
		}
		a.fileSearches[id] = search
	}
	a.fileMu.Unlock()
	return search.index.List(ctx, query)
}

func (a *App) completionFor(id string) tui.CompletionProvider {
	return func(line string, cursor int) tui.CompletionResult {
		runes := []rune(line)
		if cursor < 0 || cursor > len(runes) {
			return tui.CompletionResult{}
		}
		prefix := string(runes[:cursor])
		if strings.HasPrefix(prefix, "/") && !strings.ContainsFunc(prefix, unicode.IsSpace) {
			end := cursor
			for end < len(runes) && !unicode.IsSpace(runes[end]) {
				end++
			}
			result := tui.CompletionResult{Start: 0, End: end}
			seen := map[string]bool{}
			for _, row := range strings.Split(slashHelp, "\n") {
				fields := strings.Fields(row)
				if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") || seen[fields[0]] {
					continue
				}
				name := fields[0]
				seen[name] = true
				if strings.HasPrefix(name, strings.ToLower(prefix)) {
					result.Candidates = append(result.Candidates, tui.Completion{Value: name + " ", Label: name, Detail: strings.Join(fields[1:], " ")})
				}
			}
			sort.Slice(result.Candidates, func(i, j int) bool { return result.Candidates[i].Label < result.Candidates[j].Label })
			return result
		}
		token, ok := reference.ActiveAtToken(line, cursor)
		if !ok {
			return tui.CompletionResult{}
		}
		result := tui.CompletionResult{Start: token.Start, End: token.End}
		ctx := a.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if candidates, err := a.fileCandidates(ctx, id, token.Query); err == nil {
			for _, candidate := range candidates {
				if mention, ok := reference.FormatFileMention(candidate.Path, candidate.Kind, token.Quoted); ok {
					result.Candidates = append(result.Candidates, tui.Completion{Value: mention, Label: candidate.Path, Detail: "file · " + candidate.Kind})
				}
			}
		}
		if candidates, err := a.sessionReferences().Candidates(ctx, id, token.Query, 0); err == nil {
			for _, candidate := range candidates {
				detail := "session"
				if !candidate.SameWorkspace && candidate.CWD != "" {
					detail += " · " + candidate.CWD
				}
				result.Candidates = append(result.Candidates, tui.Completion{Value: candidate.Mention, Label: candidate.DisplayTitle, Detail: detail})
			}
		}
		return result
	}
}

func (a *App) sessionReferences() reference.Sessions {
	resolver := reference.Sessions{Store: a.store, Options: reference.SessionOptions{ContextTokens: a.referenceContextWindow, Labels: func(session core.Session) (string, string) {
		label := session.Title
		if label == "" {
			label = session.ID
		}
		display := label
		if session.CreationLabel != "" {
			display = session.CreationLabel
		}
		return label, display
	}, Spill: func(ctx context.Context, input reference.SessionSpillRequest) (reference.SessionSpillRef, error) {
		if err := ctx.Err(); err != nil {
			return reference.SessionSpillRef{}, err
		}
		store, err := a.artifactStore()
		if err != nil {
			return reference.SessionSpillRef{}, err
		}
		source := map[string]any{"kind": "session-reference", "session_id": input.SourceID, "label": input.Label}
		ref, err := store.SpillContext(input.OwnerID, source, input.Content)
		if err != nil {
			return reference.SessionSpillRef{}, err
		}
		if err := ctx.Err(); err != nil {
			return reference.SessionSpillRef{}, err
		}
		return reference.SessionSpillRef{Locator: ref.Path, ID: ref.ID, Bytes: ref.Bytes, SHA256: ref.SHA256, RetrievalHint: "Read this saved capture with the read tool: file_path=" + strconv.Quote(ref.Path) + ", using offset and limit to page through it."}, nil
	}}}
	if cfg := a.cfg.SessionReferences; cfg != nil {
		if cfg.MaxReferences != nil {
			resolver.Options.MaxReferences = *cfg.MaxReferences
		}
		if cfg.CandidateLimit != nil {
			resolver.Options.CandidateLimit = *cfg.CandidateLimit
		}
		if cfg.MaxReferenceBytes != nil {
			resolver.Options.MaxReferenceBytes = *cfg.MaxReferenceBytes
		}
		resolver.Options.ReferenceContextFraction = cfg.ReferenceContextFraction
	}
	return resolver
}

func (a *App) prepareInput(ctx context.Context, session core.Session, message core.Message) (core.Message, []core.Message, error) {
	prepared, err := a.sessionReferences().Prepare(ctx, session.ID, message)
	return prepared.Direct, prepared.Contexts, err
}

func (a *App) referenceContextWindow(ctx context.Context, id string) (int, error) {
	session, err := a.store.Get(id)
	if err != nil {
		return 0, err
	}
	provider, name := session.Provider, session.Model
	if provider == "" {
		provider = a.cfg.SelectedProvider
	}
	if name == "" {
		name = a.cfg.Model
	}
	var catalog []model.ModelInfo
	if (provider == "" || provider == "deepseek-official") && officialAPI(a.cfg.BaseURL) {
		_, token, err := credentials(a.cfg)
		if err != nil {
			return 0, err
		}
		if token != "" {
			catalog, err = model.New(model.Options{BaseURL: a.cfg.BaseURL, Token: token}).ListAccountModels(ctx)
			if err != nil {
				return 0, err
			}
		} else {
			catalog = model.BuiltinModels()
		}
	} else if raw, ok := a.cfg.ProviderProfiles[provider]; ok {
		var options model.ProviderOptions
		if err = json.Unmarshal(raw, &options); err != nil {
			return 0, err
		}
		client, err := model.NewProvider(options)
		if err != nil {
			return 0, err
		}
		catalog, err = client.ListModels(ctx)
		if err != nil {
			return 0, err
		}
	}
	for _, entry := range catalog {
		if entry.ID == name && entry.Supported {
			return entry.ContextWindow, nil
		}
	}
	return 0, nil
}

func (a *App) referenceSystemInstructions(session core.Session) string {
	for _, spec := range a.specsFor(session) {
		if spec.Name == "read" {
			return "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces."
		}
	}
	return ""
}
