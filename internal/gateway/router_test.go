package gateway_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestNewRouterRejectsEmptyRouteList(t *testing.T) {
	if _, err := gateway.NewRouter(nil); err == nil {
		t.Fatal("NewRouter accepted nil routes; want error")
	}
	if _, err := gateway.NewRouter([]gateway.Route{}); err == nil {
		t.Fatal("NewRouter accepted empty routes; want error")
	}
}

func TestNewRouterRejectsEmptyPrefix(t *testing.T) {
	_, err := gateway.NewRouter([]gateway.Route{
		{Prefix: "", Backend: mustURL(t, "http://markup-svc:8080")},
	})
	if err == nil {
		t.Fatal("NewRouter accepted empty prefix; want error")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err %q does not mention 'empty'", err)
	}
}

func TestNewRouterRejectsNilBackend(t *testing.T) {
	_, err := gateway.NewRouter([]gateway.Route{
		{Prefix: "/decide", Backend: nil},
	})
	if err == nil {
		t.Fatal("NewRouter accepted nil backend; want error")
	}
}

func TestNewRouterRejectsDuplicatePrefix(t *testing.T) {
	_, err := gateway.NewRouter([]gateway.Route{
		{Prefix: "/decide", Backend: mustURL(t, "http://markup-svc:8080")},
		{Prefix: "/admin/reload", Backend: mustURL(t, "http://markup-svc:8080")},
		{Prefix: "/decide", Backend: mustURL(t, "http://other:9090")},
	})
	if err == nil {
		t.Fatal("NewRouter accepted duplicate prefix; want error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err %q does not mention 'duplicate'", err)
	}
	if !strings.Contains(err.Error(), "/decide") {
		t.Errorf("err %q does not quote the duplicated prefix", err)
	}
}

func TestMatchReturnsRouteOnSimplePrefixMatch(t *testing.T) {
	backend := mustURL(t, "http://markup-svc:8080")
	r, err := gateway.NewRouter([]gateway.Route{
		{Prefix: "/decide", Backend: backend},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	got, ok := r.Match("/decide")
	if !ok {
		t.Fatal("Match returned false on exact-prefix path")
	}
	if got.Backend != backend {
		t.Errorf("backend = %v, want %v", got.Backend, backend)
	}
}

func TestMatchHandlesPathLongerThanPrefix(t *testing.T) {
	r, _ := gateway.NewRouter([]gateway.Route{
		{Prefix: "/decide", Backend: mustURL(t, "http://markup-svc:8080")},
	})
	if _, ok := r.Match("/decide/v2/foo"); !ok {
		t.Fatal("Match returned false on prefix-extension path")
	}
}

func TestMatchReturnsFalseWhenNoMatch(t *testing.T) {
	r, _ := gateway.NewRouter([]gateway.Route{
		{Prefix: "/decide", Backend: mustURL(t, "http://markup-svc:8080")},
	})
	got, ok := r.Match("/other/path")
	if ok {
		t.Fatalf("Match returned (%+v, true) for non-matching path; want (_, false)", got)
	}
	if got != (gateway.Route{}) {
		t.Errorf("Match returned non-zero Route on miss: %+v", got)
	}
}

// TestMatchDispatchesToLongestPrefix is the load-bearing test for
// the router's selection logic. With two overlapping routes
// ("/decide" and "/decide/v2"), a path matching both must dispatch
// to the longer prefix regardless of insertion order. The pair of
// sub-tests permutes the insertion order to confirm length wins
// over insertion order.
func TestMatchDispatchesToLongestPrefix(t *testing.T) {
	short := mustURL(t, "http://short:8080")
	long := mustURL(t, "http://long:9090")
	cases := []struct {
		name   string
		routes []gateway.Route
	}{
		{
			name: "short_then_long",
			routes: []gateway.Route{
				{Prefix: "/decide", Backend: short},
				{Prefix: "/decide/v2", Backend: long},
			},
		},
		{
			name: "long_then_short",
			routes: []gateway.Route{
				{Prefix: "/decide/v2", Backend: long},
				{Prefix: "/decide", Backend: short},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := gateway.NewRouter(tc.routes)
			if err != nil {
				t.Fatalf("NewRouter: %v", err)
			}
			got, ok := r.Match("/decide/v2/predict")
			if !ok {
				t.Fatal("Match returned false on path matching both routes")
			}
			if got.Backend != long {
				t.Errorf("backend = %v, want long (%v)", got.Backend, long)
			}

			// A path matching only the short prefix still hits short.
			got, ok = r.Match("/decide/legacy")
			if !ok {
				t.Fatal("Match returned false on short-only-matching path")
			}
			if got.Backend != short {
				t.Errorf("short-matching path returned %v, want short (%v)", got.Backend, short)
			}
		})
	}
}

func TestRoutesReturnsLengthDescendingOrder(t *testing.T) {
	r, _ := gateway.NewRouter([]gateway.Route{
		{Prefix: "/a", Backend: mustURL(t, "http://a")},
		{Prefix: "/aaaa", Backend: mustURL(t, "http://aaaa")},
		{Prefix: "/aa", Backend: mustURL(t, "http://aa")},
	})
	got := r.Routes()
	want := []string{"/aaaa", "/aa", "/a"}
	if len(got) != len(want) {
		t.Fatalf("len(routes) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Prefix != w {
			t.Errorf("routes[%d].Prefix = %q, want %q", i, got[i].Prefix, w)
		}
	}
}

// TestMatchHandlesCatchAllRoute pins the documented behavior that
// a "/" prefix matches every path. The catch-all sorts last
// (length 1, shortest) so any longer-prefix route still wins for
// its specific paths; "/" is the fallback only when nothing else
// matches.
func TestMatchHandlesCatchAllRoute(t *testing.T) {
	catchAll := mustURL(t, "http://catch-all")
	specific := mustURL(t, "http://specific")
	r, err := gateway.NewRouter([]gateway.Route{
		{Prefix: "/", Backend: catchAll},
		{Prefix: "/decide", Backend: specific},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	// /decide path -> specific (longer prefix wins).
	got, ok := r.Match("/decide")
	if !ok || got.Backend != specific {
		t.Errorf("/decide -> %v, want specific (%v)", got.Backend, specific)
	}

	// /anything-else -> catch-all.
	got, ok = r.Match("/some/other/path")
	if !ok || got.Backend != catchAll {
		t.Errorf("/some/other/path -> %v, want catchAll (%v)", got.Backend, catchAll)
	}
}

// TestMatchStableSortTieBreakingPreservesInputOrder pins that two
// routes with equal-length prefixes resolve in input order. A
// non-stable sort would let the dispatch swap arbitrarily across
// NewRouter calls with the same inputs.
func TestMatchStableSortTieBreakingPreservesInputOrder(t *testing.T) {
	a := mustURL(t, "http://a")
	b := mustURL(t, "http://b")
	r, err := gateway.NewRouter([]gateway.Route{
		{Prefix: "/aa", Backend: a},
		{Prefix: "/bb", Backend: b},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	routes := r.Routes()
	if len(routes) != 2 {
		t.Fatalf("len(routes) = %d, want 2", len(routes))
	}
	// Both have length 2; input order should be preserved:
	// /aa first, /bb second.
	if routes[0].Prefix != "/aa" || routes[1].Prefix != "/bb" {
		t.Errorf("equal-length prefixes reordered: got %q, %q; want /aa, /bb", routes[0].Prefix, routes[1].Prefix)
	}
}

// TestRoutesReturnsDefensiveCopy pins the contract that callers
// can mutate the returned slice without affecting the Router's
// internal state. A regression that returned the underlying slice
// would let a caller swap routes silently.
func TestRoutesReturnsDefensiveCopy(t *testing.T) {
	r, _ := gateway.NewRouter([]gateway.Route{
		{Prefix: "/decide", Backend: mustURL(t, "http://markup-svc:8080")},
	})
	snap := r.Routes()
	snap[0].Prefix = "/zzzz"

	again := r.Routes()
	if again[0].Prefix != "/decide" {
		t.Errorf("mutation of Routes() result leaked into Router; got %q, want %q", again[0].Prefix, "/decide")
	}
}
