package cli

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	instructions "dsgo/internal/context"
	"dsgo/internal/core"
	"dsgo/internal/webtools"
	"dsgo/internal/workflow"
)

// The deployment-owned plan section is copied from the pinned standard patch.
//
//go:embed prompts/plan.md
var officialPlanPolicy string

type promptSection struct {
	name  string
	order int
	text  string
}

func (a *App) presetSystemPrompt(s core.Session, skills []instructions.Skill) string {
	if sessionPreset(s) == "minimal" {
		return "You are a helpful software engineer assistant."
	}
	modelName := s.Model
	if modelName == "" {
		modelName = a.cfg.Model
	}
	sections := []promptSection{
		{"harness:identity", -1000, "You are an AI agent powered by DeepSeek Harness."},
		{"deployment:persona-prefix", 0, fmt.Sprintf("You are a coding agent powered by the %s model.", modelName)},
		{"tool:web", 2000, webtools.Guidance()},
		{"tool:goal", 2400, a.goalGuidance()},
		{"tool:workflow", 2600, workflow.Guidance},
		{"tool:subagent", 2800, subagentGuidance},
		{"mcp:servers", 3100, a.mcpSystemInstructions()},
		{"tools:sdk", 5000, a.ptcInstructions(s)},
	}
	// Keep the existing filesystem skill catalog available until the dedicated
	// skill-context producer is ported. Its instruction bodies remain load-on-demand.
	if len(skills) > 0 {
		var catalog strings.Builder
		catalog.WriteString("Available filesystem skills (load_skill reads their instructions):\n")
		for _, skill := range skills {
			fmt.Fprintf(&catalog, "- %s: %s [%s]\n", skill.Name, skill.Description, skill.Path)
		}
		sections = append(sections, promptSection{"context:skills", 3000, catalog.String()})
	}
	if a.planGuidanceFor(s.ID) {
		sections = append(sections, promptSection{"plan:policy", 500, officialPlanPolicy})
	}
	available := map[string]bool{}
	for _, spec := range a.specsFor(s) {
		available[spec.Name] = true
	}
	add := func(tool string, order int, text string) {
		if available[tool] {
			sections = append(sections, promptSection{"tool:" + tool, order, text})
		}
	}
	if available["read"] {
		sections = append(sections, promptSection{"context:file-reference", 900, a.referenceSystemInstructions(s)})
	}
	add("bash", 1000, "Check the [exit code: N] marker on every bash result; investigate failures before moving on.")
	add("read", 1100, "Use the read tool — not shell commands like cat — to inspect text files. Use offset and limit to continue reading large files.")
	add("write", 1200, "Read an existing file before overwriting it with write (the default fs-observation-policy requires it) and prefer edit for targeted changes.")
	add("edit", 1300, "Read a file before editing it (the default fs-observation-policy requires it), unless you just created or edited it in this session.")
	add("glob", 1400, "Use the glob tool — not shell find — to discover files by path pattern.")
	add("grep", 1500, "Use the grep tool — not shell grep or rg — to search file contents. Use read on a matched file when you need surrounding context.")
	add("job_list", 1600, "Track every background job id you start. You are notified in-session when a job finishes — do not busy-poll or sleep on one; keep working on independent steps and do not duplicate a running job's work. Before giving a final answer, collect every still-relevant job with job_output (set wait: true only when you are genuinely blocked on it), and job_kill jobs that stopped mattering.")
	sort.Slice(sections, func(i, j int) bool {
		if sections[i].order != sections[j].order {
			return sections[i].order < sections[j].order
		}
		return sections[i].name < sections[j].name
	})
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		if text := strings.TrimSpace(section.text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}
