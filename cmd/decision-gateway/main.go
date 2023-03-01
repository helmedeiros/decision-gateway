// Package main is the decision-gateway HTTP server entry point. See
// decision-gateway/ADR-0001.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/httpapi"
	"github.com/helmedeiros/decision-gateway/internal/middleware"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "decision-gateway: %v\n", err)
		os.Exit(1)
	}
}

// readyState is the package-level atomic flag the /readyz handler
// reads on every probe. Flipped to 1 after the HTTP server's
// goroutine starts; flipped back to 0 on shutdown so a drain window
// returns 503 to the kubelet while in-flight requests finish.
// atomic.Int32 (vs atomic.Bool) for the project's Go 1.18 baseline.
var readyState int32

func markReady()    { atomic.StoreInt32(&readyState, 1) }
func markNotReady() { atomic.StoreInt32(&readyState, 0) }

func isReady() (string, bool) {
	if atomic.LoadInt32(&readyState) == 1 {
		return "", true
	}
	return "gateway not yet bound to listener", false
}

// run wires the gateway. Separated from main so tests can drive it
// with a cancellable ctx, captured stdout/stderr, and synthetic
// args without spawning a real process. Mirrors the markup-svc and
// traffic-gen pattern.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("decision-gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":8090", "HTTP listen address")
	backendTimeout := fs.Duration("backend-timeout", 5*time.Second, "per-request response-header timeout on outbound requests to backends")
	var routeSpecs routeFlagList
	fs.Var(&routeSpecs, "route", "repeatable; format: PREFIX=>BACKEND_URL (e.g., /decide=>http://markup-svc:8080). See ADR-0001.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	routes, err := parseRouteFlags(routeSpecs)
	if err != nil {
		return err
	}
	router, err := gateway.NewRouter(routes)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}
	proxyHandler, err := proxy.New(router, *backendTimeout)
	if err != nil {
		return fmt.Errorf("build proxy: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/healthz", httpapi.Healthz())
	mux.Handle("/readyz", httpapi.Readyz(isReady))
	// Anything else: hand to the proxy (which itself returns 404 on
	// unmatched routes). Mounting "/" as the fallback means /healthz
	// and /readyz win their exact-path matches first.
	mux.Handle("/", proxyHandler)

	// Composition order matters: CorrelationID OUTSIDE AccessLog so
	// AccessLog reads the correlation ID from the request context
	// inside the correlation-ID frame. The matched-route value flows
	// the opposite way via the writer-side RouteRecorder interface
	// the proxy stamps. See internal/middleware/doc.go.
	handler := middleware.CorrelationID(middleware.AccessLog(stdout, nil, mux))

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	bootBoot(stdout, *listen, routes, *backendTimeout)

	serverErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	// A 50ms grace window lets ListenAndServe acquire the socket
	// before we flip readiness; without it a fast kubelet probe
	// could land between Server.Serve's bind and readyState=1 and
	// see a 503 right after a successful boot. Production kubelets
	// poll at second granularity so this is belt-and-suspenders.
	time.Sleep(50 * time.Millisecond)
	markReady()

	select {
	case <-ctx.Done():
		markNotReady()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-serverErr:
		markNotReady()
		return err
	}
}

// bootBoot emits the structured boot event on stdout describing the
// gateway's configured listen address, route table, and backend
// timeout. Aggregators key dashboards on attrs.routes[*].prefix so
// the per-route latency view groups correctly.
func bootBoot(stdout io.Writer, listen string, routes []gateway.Route, backendTimeout time.Duration) {
	routeDescs := make([]map[string]string, len(routes))
	for i, r := range routes {
		routeDescs[i] = map[string]string{
			"prefix":  r.Prefix,
			"backend": r.Backend.String(),
		}
	}
	entry := struct {
		Time  string                 `json:"time"`
		Level string                 `json:"level"`
		Msg   string                 `json:"msg"`
		Attrs map[string]interface{} `json:"attrs"`
	}{
		Time:  time.Now().UTC().Format(time.RFC3339Nano),
		Level: "info",
		Msg:   "gateway.boot",
		Attrs: map[string]interface{}{
			"listen":          listen,
			"routes":          routeDescs,
			"backend_timeout": backendTimeout.String(),
		},
	}
	_ = json.NewEncoder(stdout).Encode(entry)
}
