// vohive-mac — a minimal SIM/modem management panel for macOS, built for
// BAIWANG/Quectel-style 4G USB dongles driven over raw USB (no serial
// driver needed). Web UI: SMS sending/inbox + cellular data usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// runATCLI implements `vohive-mac at 'AT+CSQ' ...` — an ad-hoc AT command
// console over the same raw-USB channel the server uses. Only one process
// can claim the AT interface, so stop the server before using it.
func runATCLI(cmds []string) {
	if len(cmds) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vohive-mac at 'AT+CMD' ['AT+CMD2' ...]")
		os.Exit(2)
	}
	modem := NewModem()
	for _, c := range cmds {
		resp, err := modem.Cmd(c, 15*time.Second)
		fmt.Printf(">>> %s\n%s\n", c, strings.TrimSpace(resp))
		if err != nil && strings.TrimSpace(resp) == "" {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "at" {
		runATCLI(os.Args[2:])
		return
	}

	addr := flag.String("addr", "127.0.0.1:7676", "listen address")
	dataDir := flag.String("data", "data", "directory for the SQLite database")
	archiveDelete := flag.Bool("archive-delete", true,
		"delete SMS from modem/SIM storage after archiving to the database")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	store, err := OpenStore(*dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	modem := NewModem()
	traffic := NewTrafficTracker(store)
	traffic.Start()
	archiver := NewArchiver(modem, store, *archiveDelete)
	archiver.Start()
	watchdog := NewWatchdog(modem, traffic)
	watchdog.Start()

	srv := &http.Server{Addr: *addr, Handler: NewServer(modem, traffic, store, archiver).Handler()}

	go func() {
		log.Printf("vohive-mac listening on http://%s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx) //nolint:errcheck
	watchdog.Stop()
	archiver.Stop()
	traffic.Stop()
	store.Close() //nolint:errcheck
}
