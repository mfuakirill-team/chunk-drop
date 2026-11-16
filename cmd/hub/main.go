// hub — ChunkDrop's coordination server (M2, docs/ARCHITECTURE.md §1).
// A stateless WSS service that runs in Docker on a VPS behind nginx:
// presence (which devices with the same group key are online), WebRTC
// signaling, and later the relay fallback. It writes nothing to disk
// and holds everything in RAM — a restart just drops the online list
// for a few seconds while clients reconnect.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"chunkdrop/internal/hub"
)

const version = "0.1.0-m2.2"

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
