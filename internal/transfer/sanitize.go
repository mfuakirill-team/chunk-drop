package transfer

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Filename safety rules — docs/PROTOCOL.md §8 (normative, all 7).
// Every path that enters a manifest, and every path a receiver is about
// to write to disk, must pass SanitizePath.

const (
	// MaxComponentBytes caps one path component (rule 5).
	MaxComponentBytes = 255
	// MaxDepth caps directory depth (rule 5).
	MaxDepth = 16
)

// reservedNames are Windows device names that may not exist as files
// (rule 4), compared case-insensitively, with or without extension.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizePath validates and normalizes one manifest-relative path
// ("/"-separated) according to PROTOCOL.md §8 rules 1–5 and 7.
// It returns the NFC-normalized, cleaned path or an error describing
// the violated rule. It never touches the filesystem.
func SanitizePath(rel string) (string, error) {
	// Rule 2 (encoding part): NFC-normalize first so all later checks
	// see canonical byte sequences.
	p := norm.NFC.String(rel)

	// Rule 2: reject control characters and NUL anywhere.
	for _, r := range p {
		if r == 0 || unicode.Is(unicode.Cc, r) || r == 0x7F {
			return "", fmt.Errorf("control character U+%04X in path", r)
		}
	}
	if strings.ContainsRune(p, '\\') {
		return "", fmt.Errorf("backslash in path %q (\"/\" is the only separator)", rel)
	}
	// Rule 1: no absolute paths ("/…") and no drive letters ("C:…").
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("absolute path %q", rel)
	}
	if isDrivePath(p) {
		return "", fmt.Errorf("drive letter in path %q", rel)
	}

	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for i, comp := range parts {
		// Rule 1: no "." / ".." / empty components.
		if comp == "" {
			if i == 0 || i == len(parts)-1 || parts[i-1] == "" {
				// tolerate nothing: "" means "a//b" or trailing "/"
				return "", fmt.Errorf("empty path component in %q", rel)
			}
			continue // defensive; Split never yields interior empties twice
		}
		if comp == "." || comp == ".." {
			return "", fmt.Errorf("%q component in path %q", comp, rel)
		}
		if strings.ContainsRune(comp, ':') {
			return "", fmt.Errorf("colon in component %q (Windows-hostile)", comp)
		}
		// Rule 3: strip trailing dots and spaces per component
		// (Windows quirk: "name." / "name " are unusable there).
		comp = strings.TrimRight(comp, ". ")
		if comp == "" {
			return "", fmt.Errorf("component reduces to nothing after stripping dots/spaces in %q", rel)
		}
		// Rule 4: reserved device names, with or without extension.
		base := comp
		if dot := strings.IndexByte(base, '.'); dot > 0 {
			base = base[:dot]
		}
		if reservedNames[strings.ToUpper(base)] {
			return "", fmt.Errorf("reserved device name %q in path %q", comp, rel)
		}
		// Rule 5: per-component byte length cap.
		if len(comp) > MaxComponentBytes {
			return "", fmt.Errorf("component %q longer than %d bytes", comp, MaxComponentBytes)
		}
		out = append(out, comp)
	}
	// Rule 5: depth cap.
	if len(out) > MaxDepth {
		return "", fmt.Errorf("path %q deeper than %d levels", rel, MaxDepth)
	}
	clean := strings.Join(out, "/")
	if clean == "" {
		return "", fmt.Errorf("empty path")
	}
	return clean, nil
}

// isDrivePath reports whether p starts with a Windows drive letter
// ("C:", "x:\…"). We already rejected backslashes and colons-in-component,
// so this is belt-and-suspenders for callers that bypass those checks.
func isDrivePath(p string) bool {
	if len(p) < 2 {
		return false
	}
	c := p[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
		return false
	}
	return p[1] == ':'
}

// NormalizeNFC returns the NFC form of s (rule 2).
func NormalizeNFC(s string) string { return norm.NFC.String(s) }

// resolveCaseCollisions enforces rule 6 on a built file list:
// paths that differ only by case (within one manifest) are renamed
// with a numeric suffix — never silently overwritten. The engine runs
// this after building a manifest; receivers additionally validate
// via ValidateManifest, which rejects collision-bearing manifests.
//
// Renaming keeps the extension: "A.jpg", "a.jpg" → "a (1).jpg".
// It is a programming error to call this with unsanitized paths.
func resolveCaseCollisions(files []File) []File {
	seen := make(map[string]bool, len(files))
	for i := range files {
		key := strings.ToLower(files[i].Path)
		if !seen[key] {
			seen[key] = true
			continue
		}
		files[i].Path = nextFreeName(files[i].Path, seen)
	}
	return files
}

func nextFreeName(path string, seen map[string]bool) string {
	dir, name := splitPath(path)
	ext := ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		ext = name[dot:]
		name = name[:dot]
	}
	for n := 1; ; n++ {
		cand := name + fmt.Sprintf(" (%d)", n) + ext
		if dir != "" {
			cand = dir + "/" + cand
		}
		if !seen[strings.ToLower(cand)] {
			seen[strings.ToLower(cand)] = true
			return cand
		}
	}
}

func splitPath(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// hasCaseCollisions reports whether any two manifest paths collide
// case-insensitively (rule 6 check for the receiver side).
func hasCaseCollisions(files []File) bool {
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		key := strings.ToLower(f.Path)
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}
