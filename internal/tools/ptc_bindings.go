package tools

import (
	"sort"

	"dsgo/internal/core"
)

// Scoped bindings are executable only through the same owner-aware dispatcher
// as registry tools. They do not register a global tool or leak another child's
// schema into the generated SDK.
func (r *Registry) ptcBindingNames(extra []core.ToolSpec) []string {
	seen := make(map[string]bool)
	for _, spec := range append(r.Specs(), extra...) {
		if spec.Name != "" && spec.Name != "run_code" {
			seen[spec.Name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
