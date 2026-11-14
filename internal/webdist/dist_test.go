package webdist

import (
	"io"
	"strings"
	"testing"
)

// Regression test for the CWD-dependence bug: the engine must serve the
// *embedded* UI from any working directory, so FS has to be the embed
// with a real built index.html (not the fresh-clone placeholder and not
// something sniffed from disk at init time).
func TestEmbeddedIndex(t *testing.T) {
	f, err := FS.Open("index.html")
	if err != nil {
		t.Fatalf("open index.html: %v — was the binary built without `make web`?", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("index.html is empty — `make web` did not sync the build output")
	}
	if want := "<title>ChunkDrop</title>"; !strings.Contains(string(b), want) {
		t.Errorf("index.html does not contain %q; got %d bytes", want, len(b))
	}
}
