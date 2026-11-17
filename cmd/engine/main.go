// Command engine is the ChunkDrop desktop program (Windows + Linux):
// LAN HTTP server, manifest/chunk API, QR pairing, sender dashboard.
//
// Usage:
//
//	engine                start the sender dashboard server
//	engine keygen         print a fresh 32-byte base64url group key
//	engine --port 8090    override the port
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"chunkdrop/internal/activity"
	"chunkdrop/internal/api"
	"chunkdrop/internal/config"
	"chunkdrop/internal/device"
	"chunkdrop/internal/discovery"
	"chunkdrop/internal/diskspace"
	"chunkdrop/internal/hubclient"
	"chunkdrop/internal/lanip"
	"chunkdrop/internal/logx"
	"chunkdrop/internal/picker"
	"chunkdrop/internal/session"
	"chunkdrop/internal/syncmesh"
	"chunkdrop/internal/tunnel"
	"chunkdrop/internal/upload"
	"chunkdrop/internal/webui"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("engine: ")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "keygen":
			key, err := config.Keygen()
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(key)
			return
		case "version":
			printVersion()
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}

	fs := flag.NewFlagSet("engine", flag.ExitOnError)
	fs.Usage = usage // --help / -h / a bad flag all print the same help
	port := fs.Int("port", 0, "port to bind (default from config, 8090)")
	bind := fs.String("bind", "", "IP to bind + advertise in QR (default: auto)")
	logLevel := fs.String("log-level", "", "log level: debug|info|warn|error (default info)")
	debug := fs.Bool("debug", false, "debug mode — verbose logging of every step and error")
	showVersion := fs.Bool("version", false, "print the version and exit")
	noBrowser := fs.Bool("no-browser", false, "do not open the dashboard in a browser on start")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
	if *showVersion {
		printVersion()
		return
	}
	// GNU convention: after the options, at most a subcommand may remain.
	if extra := fs.Args(); len(extra) > 0 {
		fmt.Fprintf(os.Stderr, "engine: unknown command %q — try 'engine --help'\n", extra[0])
		os.Exit(2)
	}
	// --debug is shorthand for --log-level debug; both filter everything
	// through internal/logx (docs/USAGE.md §3).
	if *debug {
		*logLevel = "debug"
	}
	if *logLevel != "" {
		lvl, err := logx.ParseLevel(*logLevel)
		if err != nil {
			log.Fatal(err)
		}
		logx.SetLevel(lvl)
	}

	if err := run(*port, *bind, *noBrowser); err != nil {
		log.Fatal(err)
	}
}

func usage() {
	cfgPath, _ := config.ConfigPath()
	recvDir, _ := config.DefaultReceiveDir()
	deviceKey := ""
	if cfgPath != "" {
		deviceKey = filepath.Join(filepath.Dir(cfgPath), device.KeyFile)
	}
	fmt.Printf(`chunkdrop engine — LAN file transfer

Usage:
  engine [options]      start the sender dashboard on the LAN
  engine keygen         print a fresh group key (or set it in the dashboard)
  engine version        print the build version
  engine help           show this help

Options:
  --port N              port to bind (default: from config, 8090)
  --bind IP             IP to bind and advertise in the QR
                        (default: auto-detect; the adapter with the
                        default route wins — pin it if a VPN or virtual
                        adapter is picked instead of your real LAN IP)
  --no-browser          do not open the dashboard in a browser on start
  --debug               verbose logs: every request, chunk and step
  --log-level LEVEL     noise filter: debug|info|warn|error (default: info)
                        (--debug is shorthand for --log-level debug)
  --version             print the build version and exit
  -h, --help            show this help

Single-dash spellings (-port, -bind, …) are accepted too.

Files on this setup (%s):
  config:      %s
  device key:  %s
  received:    %s  (when receive_dir is unset)
`, runtime.GOOS, cfgPath, deviceKey, recvDir)
}

func printVersion() {
	fmt.Println("chunkdrop engine M1")
}

// Package main wiring note (#1.3): one shared activity feed drives both
// the dashboard status panel and engine console lines.
func run(portFlag int, bindFlag string, noBrowser bool) error {
	cfg, cfgPath, err := config.Load()
	if err != nil {
		return err
	}
	if portFlag != 0 {
		cfg.Port = portFlag
	}
	if cfg.Port == 0 {
		cfg.Port = config.DefaultPort
	}

	// Device identity (M2, required): a persistent Ed25519 key pair at
	// <configDir>/device.key — the same machine keeps the same device
	// ID across restarts, both on the LAN (discovery beacons) and at
	// the hub. The beacon ID *is* the device ID, so LAN peers and hub
	// presence always join by the same identity. Without the key file
	// there is no mesh identity at all — fail loudly instead of
	// half-working (the key file never lives in the sync folder: it
	// would replicate to every device).
	ident, identErr := device.LoadOrCreate(filepath.Dir(cfgPath), lanip.Hostname())
	if identErr != nil {
		return fmt.Errorf("device identity unavailable: %w — ChunkDrop needs a persistent device identity; check write permissions on %s",
			identErr, filepath.Dir(cfgPath))
	}
	deviceID := ident.ID

	if !config.UsableKey(cfg.GroupKey) {
		log.Printf("NOTE: no group key configured yet — set one in the dashboard")
		log.Printf("      (or run `engine keygen` and paste the output into %s)", cfgPath)
	}

	// LAN IP detection (M0-TASKS.md step 3): enumerate candidates,
	// auto-pick a single one, ask the user when there are several.
	candidates, err := lanip.Candidates()
	if err != nil {
		log.Printf("LAN interface enumeration failed: %v (QR links will use localhost)", err)
	}
	advertise := bindFlag
	if advertise == "" {
		advertise = cfg.BindIP
	}
	if advertise == "" {
		if pick := lanip.Pick(candidates); pick != nil {
			advertise = pick.IP
			log.Printf("LAN address: %s (%s) — auto-picked", pick.IP, pick.Interface)
		} else if len(candidates) > 1 {
			fmt.Println("Multiple network interfaces found — pick one for the QR code:")
			for i, c := range candidates {
				fmt.Printf("  [%d] %s (%s)\n", i+1, c.IP, c.Interface)
			}
			fmt.Print("Enter number (or leave empty for 0.0.0.0): ")
			var line string
			if _, err := fmt.Scanln(&line); err == nil && line != "" {
				var n int
				if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= len(candidates) {
					advertise = candidates[n-1].IP
				}
			}
		} else if len(candidates) == 0 {
			log.Printf("no LAN candidates found; binding all interfaces")
		}
	}

	// #1.3: the shared activity feed (RAM ring buffer) — sessions,
	// sync pulls/deletes, errors all land here.
	act := activity.NewLog(200)

	// One shared disk-space ledger for both incoming paths (mesh
	// pulls and phone pushes): each destination directory's own
	// filesystem is checked, in-flight transfers reserve their
	// remaining bytes, so concurrent transfers cannot overflow a disk.
	space := diskspace.New()

	// HTTP server wiring.
	sm := session.NewManager()
	ui := webui.Handler()
	srv := api.NewServer(sm, ui)
	srv.SetGroupKey(cfg.GroupKey)
	srv.Picker = pickerAdapters{}
	srv.Activity = act // #1.3: concise feed → dashboard status panel
	// Dashboard is local-only by default (it controls this PC).
	srv.AllowRemoteDashboard = cfg.AllowRemoteDashboard

	// Shared-folder state (key-independent): feeds the syncmesh
	// runner and the dashboard; a dashboard rekey does not touch it.
	var local *syncmesh.Local
	if dir, derr := syncmesh.SyncDir(); derr != nil {
		log.Printf("sync dir unavailable: %v", derr)
	} else if l, lerr := syncmesh.NewLocal(dir, lanip.Hostname(), deviceID); lerr != nil {
		log.Printf("sync scan failed: %v", lerr)
	} else {
		local = l
		srv.Sync = l
		log.Printf("sync folder: %s", dir)
		// Where the config that shapes this run lives — right after
		// the sync folder, so a user reading top-down sees both.
		log.Printf("config: %s", cfgPath)
		act.Add(activity.KindInfo, "sync folder: "+dir)
	}

	handler := srv.Handler()

	// Mesh runtime (M2): LAN discovery + hub/tunnel + sync runner,
	// built per group key. The dashboard can replace the key at
	// runtime (POST /api/key) — the mesh is stopped and rebuilt live,
	// no engine restart. One instance lives at a time (keyApplier).
	applier := &keyApplier{cfgPath: cfgPath, deps: meshDeps{
		cfg:      cfg,
		ident:    ident,
		deviceID: deviceID,
		name:     lanip.Hostname(),
		port:     cfg.Port,
		srv:      srv,
		act:      act,
		space:    space,
		local:    local,
		handler:  handler,
	}}
	applier.rekey(cfg.GroupKey)
	srv.OnKey = applier.Stage

	// Phone→PC push (M1, PROTOCOL.md §4 upload table): RAM-only upload
	// sessions, gated by the upload key minted via /api/upload/new.
	// Uploads land in the sync folder (so they propagate mesh-wide)
	// unless receive_dir overrides it. Leftover staging dirs from a
	// previous run are cleared — nothing can be in flight at startup.
	srv.Uploads = upload.NewManager()
	srv.Uploads.Space = space
	uploadDest := cfg.ReceiveDir
	if uploadDest == "" {
		if srv.Sync != nil {
			uploadDest = srv.Sync.Dir
		} else if d, derr := config.DefaultReceiveDir(); derr == nil {
			uploadDest = d
		}
	}
	srv.UploadDest = uploadDest
	srv.MaxUploadBytes = cfg.MaxUploadBytes
	srv.Uploads.CleanStale(uploadDest)
	if srv.Sync == nil || !strings.EqualFold(srv.Sync.Dir, uploadDest) {
		log.Printf("phone uploads land in: %s", uploadDest)
	}

	bindAddr := lanip.BindAddr(candidates, advertise, cfg.Port)
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		// Binding a specific interface can fail (VPN half-down); retry all.
		log.Printf("bind %s failed: %v — retrying 0.0.0.0", bindAddr, err)
		ln, err = net.Listen("tcp", net.JoinHostPort("0.0.0.0", itoa(cfg.Port)))
		if err != nil {
			return err
		}
	}

	host, _, _ := net.SplitHostPort(ln.Addr().String())
	if host == "0.0.0.0" || host == "::" || host == "" {
		if advertise != "" {
			host = advertise
		} else {
			host = "127.0.0.1"
		}
	}
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(host, itoa(cfg.Port)))

	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Session reaper: 24 h expiry enforced (PROTOCOL.md §6); the upload
	// manager rides the same tick (its TTL mirrors the session TTL).
	reapStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				sm.Reap()
				srv.Uploads.Reap()
			case <-reapStop:
				return
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	fmt.Println()
	fmt.Printf("  ChunkDrop engine ready\n")
	fmt.Printf("  dashboard:  %s\n", url)
	fmt.Printf("  receive:    %s/r/#k=<session-key>\n", strings.TrimSuffix(url, "/"))
	fmt.Printf("  upload:     %s/u/#k=<upload-key>  (make the link in the dashboard)\n", strings.TrimSuffix(url, "/"))
	if srv.Sync != nil {
		fmt.Printf("  sync dir:   %s\n", srv.Sync.Dir)
	}
	if len(candidates) > 1 {
		fmt.Printf("  (%d LAN interfaces seen — use --bind to pin one)\n", len(candidates))
	}
	fmt.Println()

	// #4: open the dashboard in the default browser when we run in a
	// desktop session; headless (SSH/service/container) just prints.
	// --no-browser skips it (servers, remote desktops, scripts).
	if !noBrowser && lanip.HasGUI() {
		if err := openBrowser(url); err != nil {
			log.Printf("browser open failed (%v) — open %s manually", err, url)
		} else {
			log.Printf("opened dashboard in browser: %s", url)
		}
	}

	// Ctrl+C / SIGTERM: cancel sessions, exit clean (no state to save —
	// sessions are RAM-only by design).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		fmt.Printf("\nengine: %v — shutting down\n", s)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	close(reapStop)
	applier.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	return nil
}

// itoa pads/reports the port for URL building.
func itoa(p int) string {
	return fmt.Sprintf("%d", p)
}

// meshDeps is everything the mesh runtime needs that does NOT change
// on a live rekey: identity, engine config (hub/stun URLs), server
// wiring, the shared-folder state and the disk-space ledger.
type meshDeps struct {
	cfg      config.Config
	ident    *device.Identity
	deviceID string
	name     string
	port     int
	srv      *api.Server
	act      *activity.Log
	space    diskspace.Ledger
	local    *syncmesh.Local
	handler  http.Handler
}

// meshRuntime is one live mesh incarnation: the discovery service,
// the hub/tunnel pair (when configured) and the syncmesh runner —
// all keyed by one group key. Built by startMesh, torn down by stop.
type meshRuntime struct {
	disc      *discovery.Service
	hub       *hubclient.Client
	tunnelMgr *tunnel.Manager
	tunnelSrv *http.Server
	stopFn    func() // extra teardown (the sync runner), chained first
}

// stop unwinds the incarnation: sync runner, hub/tunnel, discovery.
func (m *meshRuntime) stop() {
	if m.stopFn != nil {
		m.stopFn()
	}
	if m.hub != nil {
		_ = m.tunnelSrv.Close()
		m.tunnelMgr.Close()
		m.hub.Stop()
	}
	m.disc.Close()
}

// startMesh builds one mesh incarnation for a group key and starts
// it: discovery beacons go out, the hub client (re)connects, the
// syncmesh runner starts polling. Replacing the key = stop the old
// incarnation, start a new one (keyApplier.rekey).
func startMesh(key string, d meshDeps) *meshRuntime {
	m := &meshRuntime{}

	// LAN discovery (#5/#7): UDP broadcast beacons authenticated by
	// an HMAC of the group key; inactive when no key is configured.
	m.disc = discService(key, d.deviceID, d.name, d.port)
	d.srv.SetDiscovery(m.disc)
	go func() { m.disc.Run() }()

	// Hub connection (M2 step ①): online presence of the internet
	// mesh. Optional (hub_url empty = off); LAN behavior is unchanged.
	// The client reconnects forever on its own with backoff.
	if d.cfg.HubURL != "" && config.UsableKey(key) && d.ident != nil {
		m.hub = hubclient.New(d.cfg.HubURL, key, d.ident, logx.Adapter(logx.LevelInfo, "hub"))
		// m.tunnelMgr is the M2 step ② WebRTC tunnel: DataChannels
		// carrying the mesh HTTP protocol to hub peers.
		m.tunnelMgr = tunnel.New(d.deviceID, d.cfg.StunURL, m.hub.SendSignal)
		m.tunnelMgr.Logf = logx.Adapter(logx.LevelInfo, "tunnel")
		m.hub.OnSignal = m.tunnelMgr.SignalIn
		m.hub.Start()
		// The tunnel's listener serves the same mesh handler a LAN
		// peer would hit: identical requests, different transport.
		m.tunnelSrv = &http.Server{
			Handler:           d.handler,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go m.tunnelSrv.Serve(m.tunnelMgr.Listener())
		d.srv.SetHub(m.hub)
		log.Printf("hub: %s — internet mesh on", d.cfg.HubURL)
	} else if d.cfg.HubURL != "" {
		log.Printf("hub: %s configured but unavailable (no group key yet) — internet mesh off", d.cfg.HubURL)
	}

	// Shared-folder auto-transfer (#6): only when a key is configured
	// (the mesh is group-key gated — syncmesh never trusts strangers).
	if config.UsableKey(key) && d.local != nil {
		puller := &syncmesh.Puller{GroupKey: key, Logf: logx.Adapter(logx.LevelInfo, "sync"), Act: d.act, Space: d.space}
		runner := &syncmesh.Runner{
			Local:    d.local,
			Puller:   puller,
			GroupKey: key,
			Peers:    func() []syncmesh.PeerRef { return peerRefs(m, d) },
			Logf:     logx.Adapter(logx.LevelInfo, "sync"),
			Act:      d.act,
		}
		syncStop := make(chan struct{})
		go runner.Run(syncStop)
		prev := m.stopFn
		m.stopFn = func() {
			close(syncStop)
			if prev != nil {
				prev()
			}
		}
	}
	return m
}

// peerRefs builds the syncmesh peer list: LAN beacons first (the
// local route always wins — step ④ adds proper probing and per-peer
// toggles), then hub devices join over the WebRTC tunnel unless they
// are already visible on the LAN. Beacons carry the persistent device
// ID, the same string the hub presence uses — the join is exact.
func peerRefs(m *meshRuntime, d meshDeps) []syncmesh.PeerRef {
	var out []syncmesh.PeerRef
	lanSeen := map[string]bool{}
	for _, mem := range m.disc.Table().List() {
		if mem.ID == m.disc.Self().ID {
			continue
		}
		lanSeen[mem.ID] = true
		out = append(out, syncmesh.PeerRef{
			Host:     net.JoinHostPort(mem.IP, fmt.Sprintf("%d", mem.Port)),
			EngineID: mem.ID,
			Name:     mem.Name,
		})
	}
	if m.hub != nil && m.tunnelMgr != nil {
		for _, dev := range m.hub.Status().Devices {
			if dev.ID == m.disc.Self().ID || dev.ID == d.deviceID || lanSeen[dev.ID] {
				continue
			}
			out = append(out, syncmesh.PeerRef{
				Host:     tunnel.HostFor(dev.ID),
				EngineID: dev.ID,
				Name:     dev.Name,
				Client:   m.tunnelMgr.HTTPClient(dev.ID),
			})
		}
	}
	return out
}

// keySaveDelay is the engine.toml write window: the mesh rekeys the
// moment a key is submitted (instant apply), but the config write is
// deferred — correcting a typo inside the window saves only once, with
// the final value. The apply itself never waits.
const keySaveDelay = 20 * time.Second

// keyApplier applies dashboard-submitted group keys: the mesh rekeys
// immediately on submit (Stage), and the engine.toml write follows
// after keySaveDelay with the latest value (comments and other
// settings preserved via config.SaveGroupKey). Exactly one mesh
// incarnation is alive at a time; the engine shuts it down on exit.
type keyApplier struct {
	cfgPath string
	deps    meshDeps

	mu      sync.Mutex
	pending string      // latest applied key not yet saved to engine.toml
	timer   *time.Timer // fires once to save the pending key
	cur     *meshRuntime
}

// Stage applies a submitted key now (mesh rekey) and (re)arms the
// engine.toml save. Called from the HTTP handler (POST /api/key) —
// the rekey runs before the response goes out, so the dashboard sees
// the new mesh immediately.
func (k *keyApplier) Stage(key string) {
	if os.Getenv(config.DefaultGroupKeyEnv) != "" {
		log.Printf("note: %s is set — it overrides engine.toml on the next restart",
			config.DefaultGroupKeyEnv)
	}
	k.rekey(key)
	k.mu.Lock()
	k.pending = key
	if k.timer != nil {
		k.timer.Stop()
	}
	k.timer = time.AfterFunc(keySaveDelay, func() { k.save(key) })
	k.mu.Unlock()
	log.Printf("dashboard: group key applied — mesh rekeyed; engine.toml updated in ~%ds",
		int(keySaveDelay.Seconds()))
}

// save persists the pending key if it is still the latest one (a
// newer Stage re-arms the timer and takes over). Runs off the lock;
// only the pending check/clear is guarded.
func (k *keyApplier) save(key string) {
	k.mu.Lock()
	latest := k.pending
	k.mu.Unlock()
	if latest != key {
		return
	}
	if err := config.SaveGroupKey(k.cfgPath, latest); err != nil {
		log.Printf("group key persist failed: %v (applied live anyway — a restart will revert it)", err)
		return
	}
	k.mu.Lock()
	if k.pending == key {
		k.pending = ""
	}
	k.mu.Unlock()
}

// rekey swaps the mesh incarnation under the lock (also the initial
// build; cur == nil then).
func (k *keyApplier) rekey(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deps.srv.SetGroupKey(key)
	if k.cur != nil {
		k.cur.stop()
	}
	k.cur = startMesh(key, k.deps)
}

// stop tears the current incarnation down (engine shutdown) and
// flushes an unsaved pending key so a submit right before exit still
// survives the restart.
func (k *keyApplier) stop() {
	k.mu.Lock()
	if k.timer != nil {
		k.timer.Stop()
	}
	pending := k.pending
	k.pending = ""
	if k.cur != nil {
		k.cur.stop()
		k.cur = nil
	}
	k.mu.Unlock()
	if pending != "" {
		if err := config.SaveGroupKey(k.cfgPath, pending); err != nil {
			log.Printf("group key persist failed on shutdown: %v", err)
		}
	}
}

// openBrowser opens the dashboard in the default browser (#4).
// Windows: start the URL directly (ShellExecute path in cmd start).
// Linux: xdg-open (with a browser fallback list for bare setups).
// macOS: open(1).
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", strings.ReplaceAll(url, "&", "^&")).Run()
	case "darwin":
		return exec.Command("open", url).Run()
	default:
		for _, b := range []string{"xdg-open", "x-www-browser", "firefox", "chromium", "google-chrome"} {
			if _, err := exec.LookPath(b); err != nil {
				continue
			}
			if err := exec.Command(b, url).Start(); err == nil {
				return nil
			}
		}
		return fmt.Errorf("no browser launcher found")
	}
}

// pickerAdapters adapts internal/picker to the api.Picker interface.
type pickerAdapters struct{}

func (pickerAdapters) PickFiles() ([]string, error)  { return picker.Files() }
func (pickerAdapters) PickFolder() ([]string, error) { return picker.Folder() }

// discService builds the discovery service (#5/#7). The fallback for
// a keyless engine or a failed UDP bind is a real, initialized,
// inactive service (NewService with an empty key) — NOT a zero value:
// the syncmesh goroutine calls Self/Table unconditionally, and a
// zero-value Service would nil-panic there (two engines on one
// machine is the normal second-instance case: the second bind fails).
// The device ID comes from the persistent identity — beacons carry
// it, so the LAN identity always matches the hub identity (M2).
func discService(key, id, name string, port int) *discovery.Service {
	if !config.UsableKey(key) {
		return inactiveDisc(id, name, port)
	}
	s, err := discovery.NewService(key, id, name, port)
	if err != nil {
		log.Printf("discovery unavailable: %v (sync + peers panel off)", err)
		return inactiveDisc(id, name, port)
	}
	return s
}

// inactiveDisc builds the safe no-op service (no socket bound, empty
// peer table). NewService with an empty group key never fails: it
// returns before touching the network.
func inactiveDisc(id, name string, port int) *discovery.Service {
	s, err := discovery.NewService("", id, name, port)
	if err != nil {
		// Unreachable (empty key returns early), but never nil-deref.
		return nil
	}
	return s
}
