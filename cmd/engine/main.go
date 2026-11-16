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

	if err := run(*port, *bind); err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Print(`chunkdrop engine — LAN file transfer

Usage:
  engine [options]      start the sender dashboard on the LAN
  engine keygen         print a fresh group key (paste into engine.toml)
  engine version        print the build version
  engine help           show this help

Options:
  --port N              port to bind (default: from config, 8090)
  --bind IP             IP to bind and advertise in the QR
                        (default: auto-detect; pin it if a VPN or virtual
                        adapter is picked instead of your real Wi-Fi IP)
  --debug               verbose logs: every request, chunk and step
  --log-level LEVEL     noise filter: debug|info|warn|error (default: info)
                        (--debug is shorthand for --log-level debug)
  --version             print the build version and exit
  -h, --help            show this help

Single-dash spellings (-port, -bind, …) are accepted too.

Config: ~/.config/chunkdrop/engine.toml (Linux)
        %APPDATA%\chunkdrop\engine.toml (Windows)
`)
}

func printVersion() {
	fmt.Println("chunkdrop engine M1")
}

// Package main wiring note (#1.3): one shared activity feed drives both
// the dashboard status panel and engine console lines.
func run(portFlag int, bindFlag string) error {
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

	// Device identity (M2): a persistent Ed25519 key pair at
	// <configDir>/device.key — the same machine keeps the same device
	// ID across restarts, both on the LAN (discovery beacons) and at
	// the hub. The key file never lives in the sync folder (it would
	// replicate to every device). Failure only disables the internet
	// mesh; the engine keeps running.
	ident, identErr := device.LoadOrCreate(filepath.Dir(cfgPath), lanip.Hostname())
	if identErr != nil {
		log.Printf("device identity unavailable: %v (internet mesh off)", identErr)
	}

	if cfg.GroupKey == "" || strings.Contains(cfg.GroupKey, "<paste") {
		log.Printf("NOTE: no group key configured yet — run `engine keygen` and")
		log.Printf("      paste the output into %s", cfgPath)
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
	srv.GroupKey = cfg.GroupKey
	srv.Picker = pickerAdapters{}
	srv.Activity = act // #1.3: concise feed → dashboard status panel
	// Dashboard is local-only by default (it controls this PC).
	srv.AllowRemoteDashboard = cfg.AllowRemoteDashboard

	// LAN discovery (#5/#7): UDP broadcast beacons authenticated by
	// an HMAC of the group key; inactive when no key is configured.
	// Beacons carry the persistent device ID (M2): same machine = same
	// identity on the LAN and at the hub.
	deviceID := ""
	if ident != nil {
		deviceID = ident.ID
	}
	disc := discService(cfg, deviceID, lanip.Hostname(), cfg.Port)
	srv.Discovery = disc
	handler := srv.Handler()

	// Hub connection (M2 step ①): online presence of the internet
	// mesh. Optional (hub_url empty = off); LAN behavior is unchanged.
	// The client reconnects forever on its own with backoff.
	var hubStop func()
	// tunnelMgr is the M2 step ② WebRTC tunnel: DataChannels carrying
	// the mesh HTTP protocol to hub peers. nil without a hub.
	var tunnelMgr *tunnel.Manager
	var hubClient *hubclient.Client
	if cfg.HubURL != "" && ident != nil && deviceID != "" && cfg.GroupKey != "" {
		hubClient = hubclient.New(cfg.HubURL, cfg.GroupKey, ident, logx.Adapter(logx.LevelInfo, "hub"))
		tunnelMgr = tunnel.New(deviceID, cfg.StunURL, hubClient.SendSignal)
		tunnelMgr.Logf = logx.Adapter(logx.LevelInfo, "tunnel")
		hubClient.OnSignal = tunnelMgr.SignalIn
		hubClient.Start()
		// The tunnel's listener serves the same mesh handler a LAN
		// peer would hit: identical requests, different transport.
		tunnelSrv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       2 * time.Minute,
		}
		go tunnelSrv.Serve(tunnelMgr.Listener())
		srv.Hub = hubClient
		hubStop = func() {
			_ = tunnelSrv.Close()
			tunnelMgr.Close()
			hubClient.Stop()
		}
		log.Printf("hub: %s — internet mesh on", cfg.HubURL)
	} else if cfg.HubURL != "" {
		log.Printf("hub: %s configured but unavailable (%s) — internet mesh off",
			cfg.HubURL, hubDisableReason(identErr, cfg.GroupKey))
	}

	// Shared-folder auto-transfer (#6): only when a key is configured
	// (the mesh is group-key gated — syncmesh never trusts strangers).
	var syncStop = make(chan struct{})
	if cfg.GroupKey != "" && !strings.Contains(cfg.GroupKey, "<paste") {
		dir, err := syncmesh.SyncDir()
		if err != nil {
			log.Printf("sync dir unavailable: %v", err)
		} else {
			log.Printf("sync folder: %s", dir)
			local, err := syncmesh.NewLocal(dir, lanip.Hostname(), disc.Self().ID)
			if err != nil {
				log.Printf("sync scan failed: %v", err)
			} else {
				srv.Sync = local
				act.Add(activity.KindInfo, "sync folder: "+dir)
				puller := &syncmesh.Puller{GroupKey: cfg.GroupKey, Logf: logx.Adapter(logx.LevelInfo, "sync"), Act: act, Space: space}
				syncRunner := &syncmesh.Runner{
					Local:    local,
					Puller:   puller,
					GroupKey: cfg.GroupKey,
					Peers: func() []syncmesh.PeerRef {
						var out []syncmesh.PeerRef
						lanSeen := map[string]bool{}
						for _, m := range disc.Table().List() {
							if m.ID == disc.Self().ID {
								continue
							}
							lanSeen[m.ID] = true
							out = append(out, syncmesh.PeerRef{
								Host:     net.JoinHostPort(m.IP, fmt.Sprintf("%d", m.Port)),
								EngineID: m.ID,
								Name:     m.Name,
							})
						}
						// Internet mesh (M2 step ②): hub devices join
						// the peer list over the WebRTC tunnel unless
						// they are already visible on the LAN (the
						// local route always wins — step ④ adds
						// proper probing and per-peer toggles).
						if tunnelMgr != nil && hubClient != nil {
							selfID := disc.Self().ID
							for _, d := range hubClient.Status().Devices {
								if d.ID == selfID || d.ID == deviceID || lanSeen[d.ID] {
									continue
								}
								out = append(out, syncmesh.PeerRef{
									Host:     tunnel.HostFor(d.ID),
									EngineID: d.ID,
									Name:     d.Name,
									Client:   tunnelMgr.HTTPClient(d.ID),
								})
							}
						}
						return out
					},
					Logf: logx.Adapter(logx.LevelInfo, "sync"),
					Act:  act,
				}
				go syncRunner.Run(syncStop)
			}
		}
	}

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

	// Discovery beacons (#5/#7) — announce + listen until shutdown.
	go func() { disc.Run() }()

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
	if lanip.HasGUI() {
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
	close(syncStop)
	disc.Close()
	if hubStop != nil {
		hubStop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	return nil
}

// itoa pads/reports the port for URL building.
func itoa(p int) string {
	return fmt.Sprintf("%d", p)
}

// hubDisableReason explains why the internet mesh is off (log line).
func hubDisableReason(identErr error, groupKey string) string {
	switch {
	case identErr != nil:
		return "no device identity"
	case groupKey == "":
		return "no group key"
	default:
		return "unknown"
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
// The device ID comes from the persistent identity — an empty
// fallback ID keeps the keyless case harmless (nothing joins the
// mesh).
func discService(cfg config.Config, id, name string, port int) *discovery.Service {
	if cfg.GroupKey == "" || strings.Contains(cfg.GroupKey, "<paste") {
		return inactiveDisc(id, name, port)
	}
	s, err := discovery.NewService(cfg.GroupKey, id, name, port)
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
