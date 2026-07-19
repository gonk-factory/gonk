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
// band; it NEVER runs inside a test.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
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
	if *record != "" {
		rec, err := stubmodel.NewRecorder(*record, *cassette, *dir)
		if err != nil {
			slog.Error("stubmodel: recorder", "err", err)
			os.Exit(1)
		}
		defer func() {
			if err := rec.Save(); err != nil {
				slog.Error("stubmodel: save cassette", "err", err)
			}
		}()
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
	slog.Info("gonk-stubmodel listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("stubmodel", "err", err)
		os.Exit(1)
	}
}
