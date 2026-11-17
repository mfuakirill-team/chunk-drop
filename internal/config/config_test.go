package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidKey(t *testing.T) {
	good, _ := Keygen()
	for _, k := range []string{good, "simple-key-1", "aB3_-+/=.xyz", "test", "short", "has space"} {
		if !ValidKey(k) {
			t.Errorf("ValidKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{
		"",
		"quote\"key",
		"back\\slash",
		"tab\tchar",
		strings.Repeat("x", 257),
	} {
		if ValidKey(k) {
			t.Errorf("ValidKey(%q) = true, want false", k)
		}
	}
}

// UsableKey gates the mesh and the dashboard credential: a real key,
// never empty and never the fresh-install placeholder.
func TestUsableKey(t *testing.T) {
	for _, k := range []string{"test", "has space", "abc12345"} {
		if !UsableKey(k) {
			t.Errorf("UsableKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{
		"",
		"<paste `engine keygen` output here>",
		`group_key = "<paste ..."`,
	} {
		if UsableKey(k) {
			t.Errorf("UsableKey(%q) = true, want false", k)
		}
	}
}

// The dashboard writes the key into engine.toml (M2): the rewrite must
// touch only the group_key line — comments and other settings survive.
func TestSaveGroupKeyReplacesLineKeepsRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "engine.toml")
	src := `# my engine
group_key = "<paste ` + "`engine keygen`" + ` output here>"
port = 8090

# hub below
hub_url = "wss://drop.404.mn/hub/ws"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	k, _ := Keygen()
	if err := SaveGroupKey(path, k); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if !strings.Contains(s, "# my engine") || !strings.Contains(s, "# hub below") {
		t.Errorf("comments lost:\n%s", s)
	}
	if !strings.Contains(s, "port = 8090") || !strings.Contains(s, `hub_url = "wss://drop.404.mn/hub/ws"`) {
		t.Errorf("other settings lost:\n%s", s)
	}
	if strings.Contains(s, "<paste") {
		t.Errorf("old placeholder still present:\n%s", s)
	}
	want := `group_key = "` + k + `"`
	if !strings.Contains(s, want) {
		t.Errorf("new key line missing:\n%s", s)
	}
	// Exactly one group_key line.
	if n := strings.Count(s, "group_key"); n != 1 {
		t.Errorf("group_key appears %d times, want 1:\n%s", n, s)
	}
}

func TestSaveGroupKeyAppendsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "engine.toml")
	if err := os.WriteFile(path, []byte("port = 8091\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveGroupKey(path, "abc12345"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "\ngroup_key = \"abc12345\"\n") && !strings.Contains(string(b), "group_key = \"abc12345\"") {
		t.Errorf("appended key missing:\n%s", b)
	}
}

func TestSaveGroupKeyCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "engine.toml")
	if err := SaveGroupKey(path, "abc12345"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `group_key = "abc12345"`) {
		t.Errorf("template key missing:\n%s", b)
	}
}

func TestSaveGroupKeyRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "engine.toml")
	if err := SaveGroupKey(path, "bad\"quoted"); err == nil {
		t.Fatal("invalid key accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file created for invalid key: %v", err)
	}
}
