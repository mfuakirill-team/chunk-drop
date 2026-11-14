// Package discovery implements ChunkDrop LAN engine discovery: UDP
// broadcast beacons authenticated by an HMAC of the group key.
//
// A beacon is a tiny fixed-ish JSON body:
//
//	{"v":1,"id":"<engine-instance-id>","name":"<hostname>",
//	 "port":8090,"ts":1234567890,"mac":"<hex-hmac>"}
//
// `mac` = HMAC-SHA256(key=group_key, "<id>|<name>|<port>|<ts>") over
// the beacon fields — a LAN stranger without the group key cannot
// forge a member (ground rule #5: keys, not labels). Beacons carry no
// secrets (the group key itself never leaves the engine).
package discovery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"net"
	"sync"
	"time"

	"chunkdrop/internal/lanip"
)

// BeaconRate is how often an engine announces itself.
const BeaconRate = 3 * time.Second

// MemberTTL is how long a member stays listed after its last beacon.
const MemberTTL = 10 * time.Second

// Beacon is one announcement on the wire.
type Beacon struct {
	V    int    `json:"v"`
	ID   string `json:"id"`   // engine instance id (random, per process)
	Name string `json:"name"` // hostname (display)
	Port int    `json:"port"` // engine HTTP port
	TS   int64  `json:"ts"`   // unix seconds (replay window)

	MAC string `json:"mac"` // hex HMAC over the fields above
}

// Authenticator verifies/creates beacon MACs with the group key.
type Authenticator struct {
	mu  sync.Mutex
	hml func() hash.Hash // fresh HMAC hasher per use
}

// NewAuthenticator builds the shared-key MAC helper.
func NewAuthenticator(groupKey string) *Authenticator {
	gk := []byte(groupKey)
	return &Authenticator{
		hml: func() hash.Hash { return hmac.New(sha256.New, gk) },
	}
}

// MAC returns the hex HMAC of the beacon's fields (v..ts).
func (a *Authenticator) MAC(b Beacon) string {
	a.mu.Lock()
	h := a.hml()
	a.mu.Unlock()
	h.Reset()
	fmt.Fprintf(h, "%d|%s|%s|%d|%d", b.V, b.ID, b.Name, b.Port, b.TS)
	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks a beacon's MAC and freshness (±MemberTTL replay window
// keeps a captured beacon useless after re-broadcast would be stale).
func (a *Authenticator) Verify(b Beacon) bool {
	// replay window: absolute ts sanity + staleness
	now := time.Now().Unix()
	if b.TS < now-int64(MemberTTL/time.Second)-5 || b.TS > now+30 {
		return false
	}
	return hmac.Equal([]byte(a.MAC(b)), []byte(b.MAC))
}

// NewID returns a fresh engine instance id.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Table tracks discovered members (last-beacon wins per id).
type Table struct {
	mu      sync.Mutex
	members map[string]*Member
}

// Member is a discovered engine on the LAN.
type Member struct {
	Beacon
	IP        string    `json:"ip"` // source IP of the last beacon
	FirstSeen time.Time `json:"-"`
	LastSeen  time.Time `json:"-"`
}

// List returns a snapshot of live members sorted by name.
func (t *Table) List() []Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	out := make([]Member, 0, len(t.members))
	for _, m := range t.members {
		if now.Sub(m.LastSeen) > MemberTTL {
			continue // stale; not yet reaped
		}
		out = append(out, *m)
	}
	return out
}

// Observe records a beacon (trusted: caller already verified MAC).
func (t *Table) Observe(b Beacon, sourceIP string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	m, ok := t.members[b.ID]
	if !ok {
		m = &Member{FirstSeen: now}
		if t.members == nil {
			t.members = map[string]*Member{}
		}
		t.members[b.ID] = m
	}
	m.Beacon = b
	m.IP = sourceIP
	m.LastSeen = now
}

// Reap drops members not seen for a TTL.
func (t *Table) Reap() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, m := range t.members {
		if now.Sub(m.LastSeen) > 2*MemberTTL {
			delete(t.members, id)
		}
	}
}

// Service both announces this engine and listens for peers.
type Service struct {
	auth    *Authenticator
	table   *Table
	self    Beacon
	conn    *net.UDPConn
	stop    chan struct{}
	stopped sync.Once
	debugf  func(string, ...any)
}

// NewService builds the discovery service bound to :BroadcastPort.
// groupKey empty → service inactive (Verify always fails, nothing
// joins the mesh, beacons are not sent — a keyless engine is a plain
// QR sender).
func NewService(groupKey string, name string, httpPort int) (*Service, error) {
	s := &Service{
		auth:   NewAuthenticator(groupKey),
		table:  &Table{},
		self:   Beacon{V: 1, ID: NewID(), Name: name, Port: httpPort},
		stop:   make(chan struct{}),
		debugf: func(string, ...any) {},
	}
	if groupKey == "" {
		return s, nil // inactive
	}
	// One socket both sends broadcasts and receives peers': bind
	// the wildcard address so replies from every interface arrive.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: lanip.BroadcastPort})
	if err != nil {
		return nil, fmt.Errorf("discovery bind :%d: %w", lanip.BroadcastPort, err)
	}
	s.conn = conn
	return s, nil
}

// SetDebugf routes beacon diagnostics (dropped invalid MACs).
func (s *Service) SetDebugf(fn func(string, ...any)) {
	s.debugf = fn
}

// Self exposes this instance's beacon fields for API display.
func (s *Service) Self() Beacon { return s.self }

// Table exposes the member table (read side).
func (s *Service) Table() *Table { return s.table }

// HTTPAddrs returns live members as host:port endpoints for the
// syncmesh runner (self excluded — never pull from ourselves).
func (s *Service) HTTPAddrs() []string {
	var out []string
	for _, m := range s.table.List() {
		if m.ID == s.self.ID {
			continue
		}
		out = append(out, joinHostPort(m.IP, m.Port))
	}
	return out
}

// MemberByID finds a live member by engine id.
func (t *Table) MemberByID(id string) (Member, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.members[id]
	if !ok || time.Since(m.LastSeen) > MemberTTL {
		return Member{}, false
	}
	return *m, true
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

// Run blocks announcing + listening until Close. Called in a goroutine
// by the engine.
func (s *Service) Run() {
	if s.conn == nil {
		<-s.stop // inactive
		return
	}
	go s.announceLoop()
	buf := make([]byte, 2048)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
				continue // transient
			}
		}
		s.handle(buf[:n], from)
	}
}

func (s *Service) announceLoop() {
	t := time.NewTicker(BeaconRate)
	defer t.Stop()
	s.announce()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.announce()
		}
	}
}

func (s *Service) announce() {
	s.self.TS = time.Now().Unix()
	s.self.MAC = s.auth.MAC(s.self)
	b, err := json.Marshal(s.self)
	if err != nil {
		return
	}
	for _, bc := range lanip.BroadcastAddrs() {
		_, _ = s.conn.WriteToUDP(b, &net.UDPAddr{IP: net.ParseIP(bc), Port: lanip.BroadcastPort})
	}
}

func (s *Service) handle(b []byte, from *net.UDPAddr) {
	var in Beacon
	if err := json.Unmarshal(b, &in); err != nil {
		return // not ours
	}
	if in.ID == s.self.ID {
		return // our own broadcast echoed back
	}
	if !s.auth.Verify(in) {
		s.debugf("discovery: dropping beacon from %s (bad MAC/stale)", from.IP)
		return
	}
	s.table.Observe(in, from.IP.String())
}

// Close stops announcing + listening.
func (s *Service) Close() {
	s.stopped.Do(func() {
		close(s.stop)
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
}
