package ratemeter

import (
	"testing"
	"time"
)

func TestRatesBasic(t *testing.T) {
	m := New(5 * time.Second)
	m.Add("peer-a", 1000)
	m.Add("peer-a", 2000)
	m.Add("peer-b", 500)

	r := m.Rates()
	if len(r) != 2 {
		t.Fatalf("rates = %v, want 2 peers", r)
	}
	// ~3000 bytes over ≈0.5s of window → thousands of B/s, sanity band.
	if r["peer-a"] < 3000 || r["peer-a"] > 6000*10 {
		t.Errorf("peer-a rate = %f B/s, implausible", r["peer-a"])
	}
	if r["peer-b"] < 500 || r["peer-b"] > 500*10 {
		t.Errorf("peer-b rate = %f B/s, implausible", r["peer-b"])
	}
}

func TestRatesWindowExpiry(t *testing.T) {
	m := New(300 * time.Millisecond)
	m.Add("peer-a", 1000)
	if len(m.Rates()) != 1 {
		t.Fatal("fresh bytes missing from rates")
	}
	time.Sleep(350 * time.Millisecond)
	r := m.Rates()
	if len(r) != 0 {
		t.Errorf("rates after window = %v, want empty (idle peers drop out)", r)
	}
	// The forgotten peer must not resurrect.
	m.Add("peer-b", 10)
	if r := m.Rates(); len(r) != 1 {
		t.Errorf("rates = %v, want only peer-b", r)
	}
}

func TestAddIgnoresJunk(t *testing.T) {
	m := New(time.Second)
	m.Add("", 1000) // no id → no attribution
	m.Add("peer", 0)
	m.Add("peer", -5)
	if r := m.Rates(); len(r) != 0 {
		t.Errorf("rates = %v, want empty", r)
	}
}

func TestRateRampsTowardTrueRate(t *testing.T) {
	m := New(time.Second)
	// Feed a steady 1 MiB/s for ~1.2 s; the reported rate should climb
	// into a sane band around 1 MiB/s (short-window startup slack ok).
	for i := 0; i < 12; i++ {
		m.Add("peer", 1<<20/12)
		time.Sleep(100 * time.Millisecond)
	}
	got := m.Rates()["peer"]
	if got < 0.3*(1<<20) || got > 3*(1<<20) {
		t.Errorf("steady-state rate = %.0f B/s, want ≈1 MiB/s", got)
	}
}
