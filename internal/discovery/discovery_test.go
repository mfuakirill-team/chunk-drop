package discovery

import (
	"net"
	"testing"
	"time"
)

func TestAuthenticatorRoundTrip(t *testing.T) {
	a := NewAuthenticator("group-secret-group-secret-group!!")
	b := Beacon{V: 1, ID: "abc123", Name: "box", Port: 8090, TS: time.Now().Unix()}
	b.MAC = a.MAC(b)
	if !a.Verify(b) {
		t.Fatal("fresh beacon must verify")
	}
}

func TestVerifyRejectsForgedAndStale(t *testing.T) {
	a := NewAuthenticator("group-secret-group-secret-group!!")
	other := NewAuthenticator("wrong-key-wrong-key-wrong!!")
	b := Beacon{V: 1, ID: "abc123", Name: "box", Port: 8090, TS: time.Now().Unix()}

	forged := b
	forged.MAC = other.MAC(forged)
	if a.Verify(forged) {
		t.Error("forged MAC must fail")
	}

	stale := b
	stale.TS = time.Now().Add(-time.Hour).Unix()
	stale.MAC = a.MAC(stale)
	if a.Verify(stale) {
		t.Error("stale beacon must fail the replay window")
	}
}

func TestTableObserveListReap(t *testing.T) {
	tbl := &Table{}
	b := Beacon{V: 1, ID: "peer1", Name: "box1", Port: 8090, TS: time.Now().Unix()}
	tbl.Observe(b, "192.168.1.9")
	got := tbl.List()
	if len(got) != 1 || got[0].IP != "192.168.1.9" || got[0].Name != "box1" {
		t.Fatalf("list = %+v", got)
	}
	// expire: rewrite LastSeen into the past
	tbl.mu.Lock()
	tbl.members["peer1"].LastSeen = time.Now().Add(-time.Hour)
	tbl.mu.Unlock()
	if len(tbl.List()) != 0 {
		t.Error("stale member must not be listed")
	}
	tbl.Reap()
	tbl.mu.Lock()
	n := len(tbl.members)
	tbl.mu.Unlock()
	if n != 0 {
		t.Errorf("reap left %d members", n)
	}
}

func TestMemberByID(t *testing.T) {
	tbl := &Table{}
	tbl.Observe(Beacon{V: 1, ID: "p", Name: "n", Port: 8090, TS: time.Now().Unix()}, "10.0.0.1")
	if m, ok := tbl.MemberByID("p"); !ok || m.IP != "10.0.0.1" {
		t.Fatalf("MemberByID = %+v ok=%v", m, ok)
	}
	if _, ok := tbl.MemberByID("nope"); ok {
		t.Error("unknown id must not be found")
	}
}

// TestServiceLoopback: two services on the same host must discover
// each other over the loopback broadcast path (all integration that
// does not need a second machine).
func TestServiceLoopback(t *testing.T) {
	key := "test-key-test-key-test-key-test!!"
	s1, err := NewService(key, "id-one-id-one-id-one-id-one-id", "one", 18091)
	if err != nil {
		t.Skipf("cannot bind discovery port: %v", err)
	}
	defer s1.Close()
	s2, err := NewService(key, "id-two-id-two-id-two-id-two-id", "two", 18092)
	if err != nil {
		t.Skipf("cannot bind second port: %v", err)
	}
	defer s2.Close()

	go s1.Run()
	go s2.Run()
	defer func() { s1.Close(); s2.Close() }()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if len(s1.Table().List()) > 0 && len(s2.Table().List()) > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("services did not discover each other within 6s")
}

func TestSelfExcludedFromHTTPAddrs(t *testing.T) {
	s := &Service{self: Beacon{ID: "me"}, table: &Table{}}
	s.table.Observe(Beacon{V: 1, ID: "me", Name: "x", Port: 8090, TS: time.Now().Unix()}, "1.2.3.4")
	s.table.Observe(Beacon{V: 1, ID: "other", Name: "y", Port: 8090, TS: time.Now().Unix()}, "1.2.3.5")
	addrs := s.HTTPAddrs()
	if len(addrs) != 1 || addrs[0] != net.JoinHostPort("1.2.3.5", "8090") {
		t.Fatalf("addrs = %v", addrs)
	}
}
