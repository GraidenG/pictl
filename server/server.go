package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"pictl/pinctl"
	"time"
)

// socketMode allows owner and group; the group owner decides who may press the
// power button. Deliberately not world-writable.
const socketMode = 0o660

//go:embed web
var webFS embed.FS

// Handler is the route table.
func GetHandler() http.Handler {
	// create multiplexer
	mux := http.NewServeMux()

	// register handler functions with multiplexer
	mux.HandleFunc("GET /api/events/status", handlePowerLedSse)
	mux.HandleFunc("GET /api/status", handleStatus)
	mux.HandleFunc("POST /api/power/short", handlePowerShort)
	mux.HandleFunc("POST /api/power/long", handlePowerLong)
	mux.HandleFunc("POST /api/reset", handleReset)

	// the operator page, embedded so the binary is self-contained
	page, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("server: embedded web assets missing: " + err.Error())
	}
	mux.Handle("GET /", http.FileServerFS(page))

	return mux
}

func GetUnixListener(addr string) (net.Listener, error) {
	err := prepareSocket(addr)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}

	// set socket permissions
	if err = os.Chmod(addr, socketMode); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// try and cleanup any old sockets first and ensure dir exists
func prepareSocket(addr string) error {
	if err := os.MkdirAll(filepath.Dir(addr), 0o755); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	// ignore error as it is probably just that the socket doesn't exist
	// it will fail later if it needs to
	_ = os.Remove(addr)
	return nil
}

// Run starts the server, designed to be run in a goroutine.
func Run(ctx context.Context, listener net.Listener, handler http.Handler) error {
	// run server in background, use serveErr channel to await its exit if it does
	serveErr := make(chan error, 1)
	go func() { serveErr <- http.Serve(listener, handler) }()

	fmt.Println("listening on", listener.Addr())

	// wait for server to exit, or for context to be cancelled
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		} // expected error on graceful exit
		return err
	case <-ctx.Done():
		// no graceful shutdown or anything rn
		return nil
	}
}

// very basic get status, as of writing I don't plan on using it but leaving it in since it is kinda basic functionality
func handleStatus(w http.ResponseWriter, r *http.Request) {
	body := []byte(`{"on":false}`)
	if pinctl.PowerLED.GetStatus() {
		body = []byte(`{"on":true}`)
	}

	w.Header().Set("Content-Type", "application/json")
	// The page polls this once a second, so only an actual failure is worth a
	// line in the log.
	if _, err := w.Write(body); err != nil {
		log.Printf("writing status: %v", err)
	}
}

const (
	statusJsonFormat = "event: status\ndata: {\"on\": %v}\n\n"
)

func handlePowerLedSse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)
	ctx := r.Context()

	ticker := time.NewTicker(250 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data := fmt.Sprintf(statusJsonFormat, pinctl.PowerLED.GetStatus())

			_, err := fmt.Fprint(w, data)
			if err != nil {
				log.Printf("writing power LED SSE: %v", err)
				return
			}

			err = rc.Flush()
			if err != nil {
				// idk... give up
				log.Printf("flushing power LED SSE: %v", err)
				return
			}
		}
	}

}

func handlePowerShort(w http.ResponseWriter, r *http.Request) {
	press(w, pinctl.PowerSwitch.ShortPress)
}

func handlePowerLong(w http.ResponseWriter, r *http.Request) {
	press(w, pinctl.PowerSwitch.LongPress)
}

func handleReset(w http.ResponseWriter, r *http.Request) {
	press(w, pinctl.ResetSwitch.ShortPress)
}

// press runs do and maps the result onto a status code. It blocks for the full
// press duration, and does not watch r.Context(): a client disconnecting must
// not abandon a press half-done, or the line stays high.
func press(w http.ResponseWriter, do func() error) {
	err := do()
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, pinctl.ErrButtonAlreadyPressed):
		http.Error(w, "already pressed", http.StatusConflict)
	default:
		http.Error(w, "press failed", http.StatusInternalServerError)
	}
}
