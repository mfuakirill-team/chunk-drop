// hub — ChunkDrop's coordination server (M2, docs/ARCHITECTURE.md §1).
// A stateless WSS service that runs in Docker on a VPS behind nginx:
// presence (which devices with the same group key are online), WebRTC
// signaling, and later the relay fallback. It writes nothing to disk
// and holds everything in RAM — a restart just drops the online list
// for a few seconds while clients reconnect.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"chunkdrop/internal/hub"
)

const version = "0.1.0-m2.3"

func usage() {
	fmt.Print(`hub — ChunkDrop coordination server (M2)

Runs behind nginx/TLS (DEPLOY-VPS.md). RAM-only: presence lives in
memory, nothing is written to disk.

Usage:
  hub [flags]

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("hub: ")

	addr := flag.String("addr", ":8080", "listen address (nginx proxies to this)")
	debug := flag.Bool("debug", false, "verbose logging (join/leave, unknown frames)")
	itlsCert := flag.String("inner-tls-cert", "", "certificate (fullchain PEM) for the inner-TLS channel — clients that send the itls1 marker over plaintext ws:// get real TLS inside the tunnel (M3)")
	itlsKey := flag.String("inner-tls-key", "", "certificate key (PEM) for --inner-tls-cert; the same pair nginx uses on 443")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("hub " + version)
		return
	}

	h := hub.New()
	if *debug {
		h.Logf = func(format string, args ...any) { log.Printf("[hub] "+format, args...) }
	}

	// Inner-TLS channel (M3): optional. Without the pair the hub stays
	// a plain-ws service and marker clients are rejected — legacy
	// behavior untouched.
	if (*itlsCert == "") != (*itlsKey == "") {
		log.Fatalf("--inner-tls-cert and --inner-tls-key go together")
	}
	if *itlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*itlsCert, *itlsKey)
		if err != nil {
			log.Fatalf("inner TLS cert: %v", err)
		}
		h.ITLS = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		log.Printf("inner-TLS channel on: itls1 over ws:// terminates with %s", *itlsCert)
	}

	srv := &http.Server{Addr: *addr, Handler: h.Handler()}
	go func() {
		log.Printf("hub %s listening on %s", version, *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// SIGTERM (docker stop): drop every connection and exit. Presence
	// is RAM-only, so there is nothing else to flush.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	_ = srv.Close()
	h.Close()
}
