package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSCOR025QuotedPathDoesNotOfferSessionReferences(t *testing.T) {
	a, session := lifecycleApp(t, lifecycleModel(nil))
	a.session = session
	t.Cleanup(a.closeFileSearches)
	if _, err := a.store.Create(session.CWD, "quoted-session"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.CWD, "quoted file.txt"), []byte("contents"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`@"quoted`, `请看 @"quoted`} {
		result := a.completionFor(session.ID)(text, len([]rune(text)))
		file := false
		for _, candidate := range result.Candidates {
			if strings.Contains(candidate.Value, "dsh-session:") {
				t.Fatalf("quoted file query offered a Session: %+v", candidate)
			}
			file = file || candidate.Value == `@"quoted file.txt"`
		}
		if !file {
			t.Fatalf("quoted query lost file completion: %+v", result)
		}
	}
	unquoted := "@quoted"
	sessionFound := false
	for _, candidate := range a.completionFor(session.ID)(unquoted, len([]rune(unquoted))).Candidates {
		sessionFound = sessionFound || strings.Contains(candidate.Value, "dsh-session:")
	}
	if !sessionFound {
		t.Fatal("unquoted completion lost Session candidates")
	}
}
