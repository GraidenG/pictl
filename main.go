package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"pictl/pinctl"
	"pictl/server"
	"time"
)

// Where the API socket lives. Overridable so the systemd unit can point it at its
// RuntimeDirectory; the default matches deploy/pictl.service.
const defaultSocketPath = "/run/pictl/pictl.sock"

func socketPath() string {
	if p := os.Getenv("PICTL_SOCKET"); p != "" {
		return p
	}
	return defaultSocketPath
}

func main() {
	// initialize pinctl
	if err := pinctl.Initialize(); err != nil {
		fmt.Printf("pin initialization: %v\n", err)
		os.Exit(1)
	}

	ledSyncCtx, cancel := context.WithCancel(context.Background())
	// the LED status is updated by edge events, it probably won't get desynced, but this makes certain
	go pinctl.PowerLED.SyncValue(ledSyncCtx, 30*time.Second)
	defer cancel() // pretty sure this doesn't matter

	handler := server.GetHandler()
	sock := socketPath()
	listener, err := server.GetUnixListener(sock)
	if err != nil {
		_ = pinctl.CloseAll()
		log.Fatalf("initializing unix socket %s: %v", sock, err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(context.Background(), listener, handler) }()

	err = <-runErr
	if err != nil {
		log.Fatalf("server exited: %v", err)
	}

	_ = pinctl.CloseAll()
}
