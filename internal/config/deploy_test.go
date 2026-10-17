package config

import "testing"

// The documented config shows every default.
func TestDocumentedConfigIsTheDefaults(t *testing.T) {
	cfg, _, err := LoadFile("../../deploy/glovebox.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != Default() {
		t.Errorf("deploy/glovebox.toml differs from the defaults:\n%+v\n%+v", cfg, Default())
	}
}
