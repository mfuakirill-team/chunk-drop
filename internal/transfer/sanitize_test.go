package transfer

import (
	"strings"
	"testing"
)

// One test per rule of docs/PROTOCOL.md §8 — M0-TASKS.md step 6.

func TestSanitizeValidPaths(t *testing.T) {
	ok := []string{
		"file.txt",
		"photos/2026/cat.jpg",
		"dir/sub/file.tar.gz",
		"é.txt",
		"дата.txt",
		"日本語/ファイル.bin",
		strings.Repeat("a", 255),
	}
	for _, p := range ok {
		if _, err := SanitizePath(p); err != nil {
			t.Errorf("SanitizePath(%q) error: %v", p, err)
		}
	}
}

// Rule 1: traversal, absolute paths, drive letters.
func TestSanitizeRule1Traversal(t *testing.T) {
	bad := []string{
		"../etc/passwd",
		"photos/../../secret",
		"../../../x",
		"..",
		".",
		"./x",
		"/etc/passwd",
		"//etc/passwd",
		"C:/Windows/System32",
		"c:\\windows",
		"C:file.txt",
		"a/b/../c",
	}
	for _, p := range bad {
		if _, err := SanitizePath(p); err == nil {
			t.Errorf("SanitizePath(%q) accepted, want rejection", p)
		}
	}
}

// Rule 2: NFC normalization + control characters.
func TestSanitizeRule2Unicode(t *testing.T) {
	nfd := "e\u0301.txt" // é as NFD
	got, err := SanitizePath(nfd)
	if err != nil {
		t.Fatal(err)
	}
	if got != "é.txt" {
		t.Errorf("NFD not normalized to NFC: got %q", got)
	}
	bad := []string{
		"bad\x00.txt",
		"bad\nfile",
		"bad\rfilename",
		"bad\x1b[0m",
		"bad\x7f",
	}
	for _, p := range bad {
		if _, err := SanitizePath(p); err == nil {
			t.Errorf("SanitizePath(%q) accepted control char", p)
		}
	}
}

// Rule 3: trailing dots and spaces per component.
func TestSanitizeRule3TrailingDotsSpaces(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"name.", "name"},
		{"name  ", "name"},
		{"name . . ", "name"},
		{"dir./name.txt", "dir/name.txt"},
		{"dir /name.txt", "dir/name.txt"},
		{"a.b./c.d..txt ", "a.b/c.d..txt"},
		{"dir./sub./leaf.txt ", "dir/sub/leaf.txt"},
	}
	for _, c := range cases {
		got, err := SanitizePath(c.in)
		if err != nil {
			t.Errorf("SanitizePath(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("SanitizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := SanitizePath("..."); err == nil {
		t.Error("path of only dots should be rejected")
	}
	if _, err := SanitizePath("   "); err == nil {
		t.Error("path of only spaces should be rejected")
	}
}

// Rule 4: reserved device names, with and without extension.
func TestSanitizeRule4ReservedNames(t *testing.T) {
	bad := []string{
		"CON", "con", "Con.txt", "con.txt", "PRN.dat", "aux", "AUX.log",
		"NUL", "nul.bin", "com1", "COM9.jpg", "lpt1", "LPT7",
		"dir/com3.dat", "photos/com2",
	}
	for _, p := range bad {
		if _, err := SanitizePath(p); err == nil {
			t.Errorf("SanitizePath(%q) accepted reserved name", p)
		}
	}
	// Similar-but-fine names must stay allowed.
	for _, p := range []string{"console.log", "contact.txt", "lpt.txt", "com.txt", "nulled.txt"} {
		if _, err := SanitizePath(p); err != nil {
			t.Errorf("SanitizePath(%q) rejected but should pass: %v", p, err)
		}
	}
}

// Rule 5: component length + depth caps.
func TestSanitizeRule5LengthDepth(t *testing.T) {
	long := strings.Repeat("a", 256)
	if _, err := SanitizePath(long); err == nil {
		t.Error("256-byte component accepted")
	}
	deep := strings.Repeat("d/", 16) + "file.txt" // 17 components
	if _, err := SanitizePath(deep); err == nil {
		t.Error("17-deep path accepted")
	}
	deepOK := strings.Repeat("d/", 15) + "file.txt" // 16 components
	if _, err := SanitizePath(deepOK); err != nil {
		t.Errorf("16-deep path rejected: %v", err)
	}
}

// Rule 6: case-collision rename.
func TestSanitizeRule6CaseCollision(t *testing.T) {
	files := []File{
		{Path: "A.jpg"},
		{Path: "a.jpg"},
	}
	out := ResolveCaseCollisions(files)
	if out[1].Path == "a.jpg" {
		t.Fatalf("collision not renamed: %v", out)
	}
	if !strings.HasPrefix(out[1].Path, "a (1)") || !strings.HasSuffix(out[1].Path, ".jpg") {
		t.Errorf("renamed path keeps name/extension: got %q", out[1].Path)
	}
	if out[0].Path != "A.jpg" {
		t.Errorf("first file must keep its name, got %q", out[0].Path)
	}
	files = []File{
		{Path: "X/a.txt"},
		{Path: "x/A.txt"},
	}
	out = ResolveCaseCollisions(files)
	if out[1].Path == "x/A.txt" {
		t.Errorf("cross-directory case collision not resolved: %v", out)
	}
}

// Rule 7: receiver-side re-validation — ValidateManifest rejects
// manifest paths that fail the rules.
func TestSanitizeRule7ReceiverRecheck(t *testing.T) {
	m := NewManifest("s")
	m.ChunkSize = DefaultChunkSize
	m.Files = []File{{Index: 0, Path: "../escape.txt", Size: 1, ChunkCount: 1, Hashes: []string{strings.Repeat("0", 64)}}}
	if err := ValidateManifest(m); err == nil {
		t.Error("ValidateManifest accepted a traversal path")
	}
	m.Files = []File{{Index: 0, Path: "con.txt", Size: 1, ChunkCount: 1, Hashes: []string{strings.Repeat("0", 64)}}}
	if err := ValidateManifest(m); err == nil {
		t.Error("ValidateManifest accepted a reserved name")
	}
}
