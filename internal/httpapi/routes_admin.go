package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
	"github.com/helmedeiros/decision-gateway/internal/proxy"
)

// RoutesAdmin mounts on POST/GET /admin/routes. POST accepts a JSON
// body {"routes":[{"prefix":"...","backend":"..."}]} and atomically
// replaces the active route table via the proxy Holder; GET returns
// the current table. Invalid bodies / invalid URLs / NewRouter
// validation failures all return 400 with a JSON error body and
// leave the old table serving. See ADR-0008.
func RoutesAdmin(h *proxy.Holder, errLog io.Writer) http.Handler {
	if h == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusInternalServerError, "routes admin not configured")
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeRoutes(w, h.Routes())
		case http.MethodPost:
			postRoutes(w, r, h, errLog)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

type routeSpec struct {
	Prefix  string `json:"prefix"`
	Backend string `json:"backend"`
}

type routesBody struct {
	Routes []routeSpec `json:"routes"`
}

func postRoutes(w http.ResponseWriter, r *http.Request, h *proxy.Holder, errLog io.Writer) {
	var body routesBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if len(body.Routes) == 0 {
		writeError(w, http.StatusBadRequest, "routes array is required and must not be empty")
		return
	}
	routes := make([]gateway.Route, 0, len(body.Routes))
	for _, s := range body.Routes {
		u, err := url.Parse(s.Backend)
		if err != nil || u.Scheme == "" || u.Host == "" {
			writeError(w, http.StatusBadRequest, "invalid backend URL: "+s.Backend)
			return
		}
		routes = append(routes, gateway.Route{Prefix: s.Prefix, Backend: u})
	}
	router, err := gateway.NewRouter(routes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "router validation: "+err.Error())
		return
	}
	if err := h.Replace(router); err != nil {
		writeError(w, http.StatusInternalServerError, "replace: "+err.Error())
		return
	}
	writeRoutes(w, h.Routes())
}

func writeRoutes(w http.ResponseWriter, routes []gateway.Route) {
	out := make([]routeSpec, 0, len(routes))
	for _, r := range routes {
		out = append(out, routeSpec{Prefix: r.Prefix, Backend: r.Backend.String()})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(routesBody{Routes: out})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
