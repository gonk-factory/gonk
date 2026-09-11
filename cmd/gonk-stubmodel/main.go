// Command gonk-stubmodel runs the deterministic stub model server as a container,
// for the component (L2) and e2e (L3) suites, where LiteLLM must reach it over the
// network. Tests drive it through /_control/script and /_control/requests.
//
// It has no auth: it is only ever reachable from a test network, it holds nothing,
// and it must never be deployed anywhere real. The image is tagged with the run id
// and is never pushed to a shared registry.
//
// With -record <upstream-url> it instead proxies each request to a real upstream
// and captures a scrubbed cassette (AD-2). Record mode is run by a human, out of
// band; it NEVER runs inside a test. See `make refresh-cassette` (Makefile) for
// the documented, credentialed way to drive it.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitlab.orac.local/agentic/gonk-project/test/stubmodel"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	record := flag.String("record", "", "record mode: proxy to this upstream base URL and capture a cassette")
	cassette := flag.String("cassette", "recorded", "cassette name to write in record mode")
	dir := flag.String("cassette-dir", "test/stubmodel/cassettes", "directory to write cassettes into")
	flag.Parse()

	var handler http.Handler = stubmodel.New()
	var rec *stubmodel.Recorder
	if *record != "" {
		var err error
		rec, err = stubmodel.NewRecorder(*record, *cassette, *dir)
		if err != nil {
			slog.Error("stubmodel: recorder", "err", err)
			os.Exit(1)
		}
		slog.Warn("gonk-stubmodel RECORD mode: proxying to a real upstream", "upstream", *record, "cassette", *cassette)
		handler = rec
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		// NO WriteTimeout: Step.Hang deliberately never responds, and a server-side
		// write timeout would turn that into a server error instead of the CLIENT
		// timeout the infra-failure tests need.
	}

	// A record-mode run is stopped by a human with Ctrl-C. The DEFAULT SIGINT
	// handler kills the process immediately -- no deferred func ever runs -- so
	// a bare `defer rec.Save()` (the previous shape of this function) silently
	// discarded every captured entry on the one path a human actually uses to
	// stop it. This was never caught because nothing has ever driven the -record
	// flag end-to-end: cassettes/triage.json was hand-authored, not recorded
	// (see docs/reviews/2026-09-08-delivery-plan.md T-19 notes). Trap the signal,
	// shut the server down cleanly, THEN save.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("gonk-stubmodel listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			slog.Error("stubmodel", "err", err)
			saveIfRecording(rec)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("gonk-stubmodel: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("stubmodel: shutdown", "err", err)
		}
		<-errCh // wait for ListenAndServe's goroutine to actually exit
	}

	if !saveIfRecording(rec) {
		os.Exit(1)
	}
}

// saveIfRecording writes the captured cassette when running in record mode. It
// is a no-op (success) in normal replay mode, where rec is nil.
func saveIfRecording(rec *stubmodel.Recorder) bool {
	if rec == nil {
		return true
	}
	if err := rec.Save(); err != nil {
		slog.Error("stubmodel: save cassette", "err", err)
		return false
	}
	slog.Info("gonk-stubmodel: cassette saved")
	return true
}
