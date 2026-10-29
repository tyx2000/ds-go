package state

import "testing"

func TestReferenceSettingsPreserveExplicitEmptyExclusions(t *testing.T) {
	cfg := DefaultConfig()
	empty := []string{}
	fraction := 0.0
	cfg.FileReferences = &FileReferenceSettings{ExcludedDirectories: &empty}
	cfg.SessionReferences = &SessionReferenceSettings{ReferenceContextFraction: &fraction}
	root := t.TempDir()
	if err := SaveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.FileReferences == nil || loaded.FileReferences.ExcludedDirectories == nil || len(*loaded.FileReferences.ExcludedDirectories) != 0 {
		t.Fatal("explicit no-exclusions setting changed to defaults")
	}
	if loaded.SessionReferences == nil || loaded.SessionReferences.ReferenceContextFraction == nil || *loaded.SessionReferences.ReferenceContextFraction != 0 {
		t.Fatal("explicit zero reference fraction was lost")
	}
}

func TestReferenceSettingsRejectInvalidLimitsBeforeStartup(t *testing.T) {
	zero, four := 0, 4
	for _, mutate := range []func(*Config){
		func(c *Config) { c.FileReferences = &FileReferenceSettings{MaxEntries: &zero} },
		func(c *Config) {
			names := []string{"../escape"}
			c.FileReferences = &FileReferenceSettings{ExcludedDirectories: &names}
		},
		func(c *Config) { c.SessionReferences = &SessionReferenceSettings{MaxReferences: &four} },
		func(c *Config) { c.SessionReferences = &SessionReferenceSettings{MaxReferenceBytes: &zero} },
	} {
		cfg := DefaultConfig()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("invalid reference settings accepted")
		}
	}
}
