package main

import (
	"strings"
	"testing"
)

func TestParseRouteFlagsRejectsEmpty(t *testing.T) {
	if _, err := parseRouteFlags(nil); err == nil {
		t.Fatal("parseRouteFlags accepted nil specs; want error")
	}
}

func TestParseRouteFlagsRejectsMissingArrow(t *testing.T) {
	_, err := parseRouteFlags([]string{"/decide:http://markup-svc:8080"})
	if err == nil {
		t.Fatal("accepted missing => separator; want error")
	}
	if !strings.Contains(err.Error(), "=>") {
		t.Errorf("err %q does not mention the => separator", err)
	}
}

func TestParseRouteFlagsRejectsEmptyPrefix(t *testing.T) {
	_, err := parseRouteFlags([]string{"=>http://markup-svc:8080"})
	if err == nil {
		t.Fatal("accepted empty prefix; want error")
	}
}

func TestParseRouteFlagsRejectsPrefixWithoutSlash(t *testing.T) {
	_, err := parseRouteFlags([]string{"decide=>http://markup-svc:8080"})
	if err == nil {
		t.Fatal("accepted prefix without leading slash; want error")
	}
}

func TestParseRouteFlagsRejectsBackendWithoutScheme(t *testing.T) {
	// "just-a-host" url.Parse treats as a relative URL: Scheme=""
	// and Path="just-a-host"; the scheme check fires.
	_, err := parseRouteFlags([]string{"/decide=>just-a-host"})
	if err == nil {
		t.Fatal("accepted backend without scheme; want error")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err %q does not mention the scheme requirement", err)
	}
}

// TestParseRouteFlagsRejectsHostPortLookingLikeScheme pins the
// behavior on the ambiguous "host:port" form. url.Parse treats
// "markup-svc:8080" as Scheme="markup-svc" / Opaque="8080" with
// no host -- the host check rejects it (the scheme check passes
// the bogus scheme). Operators who type the bare host:port get
// a "no host" error pointing at the URL they wrote, which is
// actionable even if the underlying reason is subtle.
func TestParseRouteFlagsRejectsHostPortLookingLikeScheme(t *testing.T) {
	_, err := parseRouteFlags([]string{"/decide=>markup-svc:8080"})
	if err == nil {
		t.Fatal("accepted bare host:port backend; want error")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("err %q does not mention the host requirement", err)
	}
}

func TestParseRouteFlagsRejectsBackendWithoutHost(t *testing.T) {
	_, err := parseRouteFlags([]string{"/decide=>http://"})
	if err == nil {
		t.Fatal("accepted backend without host; want error")
	}
}

func TestParseRouteFlagsAcceptsValidSingleRoute(t *testing.T) {
	routes, err := parseRouteFlags([]string{"/decide=>http://markup-svc:8080"})
	if err != nil {
		t.Fatalf("parseRouteFlags: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("len(routes) = %d, want 1", len(routes))
	}
	if routes[0].Prefix != "/decide" {
		t.Errorf("prefix = %q, want /decide", routes[0].Prefix)
	}
	if routes[0].Backend.Host != "markup-svc:8080" {
		t.Errorf("backend.Host = %q, want markup-svc:8080", routes[0].Backend.Host)
	}
	if routes[0].Backend.Scheme != "http" {
		t.Errorf("backend.Scheme = %q, want http", routes[0].Backend.Scheme)
	}
}

func TestParseRouteFlagsAcceptsMultipleRoutes(t *testing.T) {
	routes, err := parseRouteFlags([]string{
		"/decide=>http://markup-svc:8080",
		"/admin/reload=>http://markup-svc:8080",
		"/decide/v2=>http://markup-v2:8080",
	})
	if err != nil {
		t.Fatalf("parseRouteFlags: %v", err)
	}
	if len(routes) != 3 {
		t.Fatalf("len(routes) = %d, want 3", len(routes))
	}
	if routes[0].Prefix != "/decide" || routes[1].Prefix != "/admin/reload" || routes[2].Prefix != "/decide/v2" {
		t.Errorf("prefixes = %q,%q,%q; want /decide,/admin/reload,/decide/v2",
			routes[0].Prefix, routes[1].Prefix, routes[2].Prefix)
	}
}

func TestParseRouteFlagsAcceptsHttpsBackend(t *testing.T) {
	routes, err := parseRouteFlags([]string{"/decide=>https://markup-svc.example.com:8443"})
	if err != nil {
		t.Fatalf("parseRouteFlags: %v", err)
	}
	if routes[0].Backend.Scheme != "https" {
		t.Errorf("backend.Scheme = %q, want https", routes[0].Backend.Scheme)
	}
}

func TestRouteFlagListSetAppendsEachOccurrence(t *testing.T) {
	var list routeFlagList
	_ = list.Set("/a=>http://a")
	_ = list.Set("/b=>http://b")
	if len(list) != 2 {
		t.Errorf("len(list) = %d after 2 Sets, want 2", len(list))
	}
	if list[0] != "/a=>http://a" || list[1] != "/b=>http://b" {
		t.Errorf("list = %v, want preserved order", []string(list))
	}
}

func TestRouteFlagListStringJoinsWithComma(t *testing.T) {
	list := routeFlagList{"/a=>http://a", "/b=>http://b"}
	if got := list.String(); got != "/a=>http://a,/b=>http://b" {
		t.Errorf("String() = %q, want comma-joined", got)
	}
}
