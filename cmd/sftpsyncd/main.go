package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/r1chjames/sftp-sync/internal/daemon"
)

func main() {
	d := daemon.New()
	if err := d.Start(); err != nil {
		log.Fatalf("daemon start: %v", err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Println("signal received, shutting down...")
		d.Shutdown()
	}()

	if err := d.ServeAPI(); err != nil {
		log.Fatalf("API server: %v", err)
	}
	log.Println("daemon stopped")
}
