package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/httpapi"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
)

func newHolder(t *testing.T, initialBackend string) *proxy.Holder {
	t.Helper()
	u, _ := url.Parse(initialBackend)
	router, _ := gateway.NewRouter([]gateway.Route{{Prefix: "/", Backend: u}})
	h, err := proxy.NewHolder(router, proxy.BuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRoutesAdmin_GET_ReturnsCurrentTable(t *testing.T) {
	h := newHolder(t, "http://backend-a:8080")
	handler := httpapi.RoutesAdmin(h, io.Discard)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/routes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Routes []struct{ Prefix, Backend string } `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 1 || got.Routes[0].Prefix != "/" || got.Routes[0].Backend != "http://backend-a:8080" {
		t.Errorf("got = %+v", got.Routes)
	}
}

func TestRoutesAdmin_POST_SwapsTable(t *testing.T) {
	h := newHolder(t, "http://backend-a:8080")
	handler := httpapi.RoutesAdmin(h, io.Discard)

	body := `{"routes":[{"prefix":"/decide","backend":"http://backend-b:9000"}]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/routes", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	routes := h.Routes()
	if len(routes) != 1 || routes[0].Prefix != "/decide" || routes[0].Backend.String() != "http://backend-b:9000" {
		t.Errorf("post-replace = %+v", routes)
	}
}

func TestRoutesAdmin_POST_RejectsInvalidBackend(t *testing.T) {
	h := newHolder(t, "http://backend-a:8080")
	handler := httpapi.RoutesAdmin(h, io.Discard)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/routes",
		strings.NewReader(`{"routes":[{"prefix":"/x","backend":"not-a-url"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	// Original table should still be active.
	if h.Routes()[0].Backend.String() != "http://backend-a:8080" {
		t.Errorf("table swapped on validation failure")
	}
}

func TestRoutesAdmin_POST_RejectsEmptyArray(t *testing.T) {
	h := newHolder(t, "http://backend-a:8080")
	handler := httpapi.RoutesAdmin(h, io.Discard)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/routes",
		strings.NewReader(`{"routes":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRoutesAdmin_WrongMethodReturns405(t *testing.T) {
	h := newHolder(t, "http://backend-a:8080")
	handler := httpapi.RoutesAdmin(h, io.Discard)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/admin/routes", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q", got)
	}
}
