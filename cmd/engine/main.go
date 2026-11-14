// Command engine is the ChunkDrop desktop program (Windows + Linux):
// LAN HTTP server, manifest/chunk API, QR pairing, sender dashboard.
//
// Usage:
//
//	engine            start the sender dashboard server
//	engine keygen     print a fresh 32-byte base64url group key
//	engine -port 8090 override the port
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
	"os/signal"
	"strings"
	"syscall"
	"time"

	"chunkdrop/internal/api"
	"chunkdrop/internal/config"
	"chunkdrop/internal/lanip"
	"chunkdrop/internal/session"
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
			fmt.Println("chunkdrop engine M0")
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}

	fs := flag.NewFlagSet("engine", flag.ExitOnError)
	port := fs.Int("port", 0, "port to bind (default from config, 8090)")
	bind := fs.String("bind", "", "IP to bind + advertise in QR (default: auto)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}

	if err := run(*port, *bind); err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Print(`chunkdrop engine

Usage:
  engine                  start the sender dashboard on the LAN
  engine keygen          print a fresh group key (paste into engine.toml)
  engine -port 8090      override the port
  engine -bind 192.168.1.20
                          force the advertised IP (VPN adapter workaround)

Config: ~/.config/chunkdrop/engine.toml (Linux)
        %APPDATA%\chunkdrop\engine.toml (Windows)
`)
}

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

	// HTTP server wiring.
	sm := session.NewManager()
	ui := webui.Handler()
	srv := api.NewServer(sm, ui)
	srv.GroupKey = cfg.GroupKey
	handler := srv.Handler()

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

	// Session reaper: 24 h expiry enforced (PROTOCOL.md §6).
	reapStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				sm.Reap()
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
	fmt.Printf("  receive:    %sr/#k=<session-key>\n", url)
	if len(candidates) > 1 {
		fmt.Printf("  (%d LAN interfaces seen — use -bind to pin one)\n", len(candidates))
	}
	fmt.Println()

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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	return nil
}

func itoa(p int) string {
	return fmt.Sprintf("%d", p)
}
