package device

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

// The identity file is the device's long-term credential: it must
// persist, survive reloads with the same fingerprint, refuse corrupt
// content, and never broaden file permissions.
func TestLoadOrCreatePersists(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreate(dir, "pc-one")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(id1.Priv) != ed25519.PrivateKeySize || len(id1.Pub) != ed25519.PublicKeySize {
		t.Fatalf("bad key sizes: priv=%d pub=%d", len(id1.Priv), len(id1.Pub))
	}
	if id1.ID != Fingerprint(id1.Pub) || len(id1.ID) != 64 {
		t.Errorf("ID %q is not the public key fingerprint", id1.ID)
	}
	if id1.Name != "pc-one" {
		t.Errorf("name = %q", id1.Name)
	}

	// Reload: same identity, not a fresh key.
	id2, err := LoadOrCreate(dir, "pc-one")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id2.ID != id1.ID {
		t.Errorf("reload changed the identity: %s != %s", id1.ID, id2.ID)
	}

	// Key file: 0600.
	fi, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
}

func TestLoadOrCreateRejectsCorruptKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFile)
	if err := os.WriteFile(path, []byte("definitely-not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "x"); err == nil {
		t.Fatal("corrupt key accepted")
	}
	// And it must not have been overwritten.
	raw, _ := os.ReadFile(path)
	if string(raw) != "definitely-not-a-key\n" {
		t.Error("corrupt key file was modified")
	}
}

func TestSignVerify(t *testing.T) {
	id, err := LoadOrCreate(t.TempDir(), "signer")
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("chunkdrop-hello|tag|id|pub|1700000000")
	sig := id.Sign(msg)
	if !Verify(id.Pub, msg, sig) {
		t.Error("valid signature rejected")
	}
	if Verify(id.Pub, append(msg, 'x'), sig) {
		t.Error("modified payload accepted")
	}
	if !VerifyID(id.ID, id.Pub, msg, sig) {
		t.Error("VerifyID rejected its own identity")
	}
	// Wrong ID (spoof attempt): fingerprint mismatch → rejected.
	if VerifyID("ffff"+id.ID[4:], id.Pub, msg, sig) {
		t.Error("VerifyID accepted a spoofed device id")
	}
}
