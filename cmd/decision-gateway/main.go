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
	gwmetrics "github.com/helmedeiros/decision-gateway/internal/observability/metrics"
	gwotel "github.com/helmedeiros/decision-gateway/internal/observability/otel"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
	"go.opentelemetry.io/otel/trace"
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
	upstreamMaxIdleConns := fs.Int("upstream-max-idle-conns", 128, "per-backend idle keep-alive pool size (default 128; stdlib default of 2 forces constant TCP open+close at typical platform QPS). See ADR-0005.")
	upstreamIdleTimeout := fs.Duration("upstream-idle-timeout", 90*time.Second, "how long an idle keep-alive connection stays in the pool before close (90s default matches http.DefaultTransport; keeps the gateway under typical NAT timeouts at ~5min). See ADR-0005.")
	upstreamH2C := fs.Bool("upstream-h2c", false, "speak HTTP/2 over cleartext to upstreams (markup-svc v0.1.11+). Multiplexes many in-flight requests over one TCP connection; pool-sizing flags above become moot when on. See ADR-0006.")
	metricsEnabled := fs.Bool("metrics-enabled", false, "expose Prometheus /metrics with gateway_requests_total counter + gateway_request_duration_seconds histogram labeled by method / route / status. See ADR-0007.")
	routesAdmin := fs.Bool("routes-admin", false, "mount POST/GET /admin/routes for hot-replacing the active route table without restart (see ADR-0008). Requires a proxy.Holder wired into the request path -- adds ~30 ns RLock per request.")
	otelEnabled := fs.Bool("otel-enabled", false, "emit OpenTelemetry spans + propagate W3C trace context to upstreams via OTLP gRPC; reads OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME etc. per the OTel SDK conventions. See ADR-0002.")
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

	var tracer trace.Tracer
	var transportWrapper func(http.RoundTripper) http.RoundTripper
	if *otelEnabled {
		t, shutdown, err := gwotel.Bootstrap(ctx, "github.com/helmedeiros/decision-gateway/cmd/decision-gateway")
		if err != nil {
			return fmt.Errorf("otel bootstrap: %w", err)
		}
		tracer = t
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdown(shutdownCtx)
		}()
		transportWrapper = func(rt http.RoundTripper) http.RoundTripper {
			return &gwotel.InstrumentedTransport{Tracer: tracer, Inner: rt}
		}
	}

	pool := proxy.PoolConfig{
		MaxIdleConnsPerHost: *upstreamMaxIdleConns,
		IdleConnTimeout:     *upstreamIdleTimeout,
	}
	protocol := proxy.UpstreamHTTP1
	if *upstreamH2C {
		protocol = proxy.UpstreamH2C
	}
	buildCfg := proxy.BuildConfig{
		BackendTimeout:   *backendTimeout,
		Pool:             pool,
		Protocol:         protocol,
		TransportWrapper: transportWrapper,
	}

	var proxyHandler http.Handler
	var routesHolder *proxy.Holder
	if *routesAdmin {
		h, err := proxy.NewHolder(router, buildCfg)
		if err != nil {
			return fmt.Errorf("build proxy holder: %w", err)
		}
		routesHolder = h
		proxyHandler = h
	} else {
		h, err := proxy.New(router, buildCfg.BackendTimeout, buildCfg.Pool, buildCfg.Protocol, buildCfg.TransportWrapper)
		if err != nil {
			return fmt.Errorf("build proxy: %w", err)
		}
		proxyHandler = h
	}

	var metricsSink *gwmetrics.Sink
	var metricsHandler http.Handler
	if *metricsEnabled {
		metricsSink, metricsHandler = gwmetrics.New()
	}

	mux := http.NewServeMux()
	mux.Handle("/healthz", httpapi.Healthz())
	mux.Handle("/readyz", httpapi.Readyz(isReady))
	if metricsHandler != nil {
		mux.Handle("/metrics", metricsHandler)
	}
	if routesHolder != nil {
		mux.Handle("/admin/routes", httpapi.RoutesAdmin(routesHolder, stderr))
	}
	mux.Handle("/", proxyHandler)

	// Composition order: CorrelationID outermost so the ID is in
	// context for every inner frame; Tracing (when on) opens the
	// span next so it sees the correlation ID; Metrics (when on)
	// wraps AccessLog so its method/route/status read the same
	// values AccessLog writes; AccessLog innermost so the span
	// covers its window.
	var inner = middleware.AccessLog(stdout, nil, mux)
	if metricsSink != nil {
		inner = metricsSink.Middleware(inner)
	}
	if tracer != nil {
		inner = gwotel.Middleware(tracer, inner)
	}
	handler := middleware.CorrelationID(inner)

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
