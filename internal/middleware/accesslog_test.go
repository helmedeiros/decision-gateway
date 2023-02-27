package middleware_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/helmedeiros/decision-gateway/internal/middleware"
)

// accessLine is the on-wire JSON shape AccessLog emits. Tests
// decode every captured line through this struct so a field-tag
// drift fails the test before any aggregator hits it.
type accessLine struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Attrs struct {
		Method        string  `json:"method"`
		Path          string  `json:"path"`
		Status        int     `json:"status"`
		DurationMS    float64 `json:"duration_ms"`
		Route         string  `json:"route,omitempty"`
		CorrelationID string  `json:"correlation_id,omitempty"`
	} `json:"attrs"`
}

func TestAccessLogEmitsExpectedJSONShape(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.AccessLog(&buf, nil, inner)

	req := httptest.NewRequest(http.MethodPost, "/decide", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line accessLine
	if err := json.NewDecoder(&buf).Decode(&line); err != nil {
		t.Fatalf("decode: %v (raw=%s)", err, buf.String())
	}
	if line.Msg != "gateway.access" {
		t.Errorf("msg = %q, want gateway.access", line.Msg)
	}
	if line.Level != "info" {
		t.Errorf("level = %q, want info", line.Level)
	}
	if line.Attrs.Method != "POST" {
		t.Errorf("method = %q, want POST", line.Attrs.Method)
	}
	if line.Attrs.Path != "/decide" {
		t.Errorf("path = %q, want /decide", line.Attrs.Path)
	}
	if line.Attrs.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", line.Attrs.Status)
	}
}

func TestAccessLogCapturesNon200Status(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	h := middleware.AccessLog(&buf, nil, inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide", nil))

	var line accessLine
	_ = json.Unmarshal(buf.Bytes(), &line)
	if line.Attrs.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", line.Attrs.Status)
	}
}

func TestAccessLogCapturesImplicit200WhenWriteWithoutWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No WriteHeader: Go's http handlers default to 200 once
		// Write is called.
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := middleware.AccessLog(&buf, nil, inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide", nil))

	var line accessLine
	_ = json.Unmarshal(buf.Bytes(), &line)
	if line.Attrs.Status != http.StatusOK {
		t.Errorf("implicit-200 status = %d, want 200", line.Attrs.Status)
	}
}

func TestAccessLogIgnoresSecondWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError) // ignored
	})
	h := middleware.AccessLog(&buf, nil, inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide", nil))

	var line accessLine
	_ = json.Unmarshal(buf.Bytes(), &line)
	if line.Attrs.Status != http.StatusOK {
		t.Errorf("status = %d after WriteHeader twice; want 200 (first wins)", line.Attrs.Status)
	}
}

func TestAccessLogReadsRouteAndCorrelationID(t *testing.T) {
	var buf bytes.Buffer
	// Inner handler simulates what the proxy adapter does:
	// type-asserts the writer as a RouteRecorder and stamps the
	// matched prefix. AccessLog reads the route off the wrapper
	// after the inner returns, plus the correlation ID off the
	// outer request context (which the CorrelationID middleware
	// stashed before invoking AccessLog).
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if rec, ok := w.(middleware.RouteRecorder); ok {
			rec.SetMatchedRoute("/decide")
		}
		w.WriteHeader(http.StatusOK)
	})
	// Composition: CorrelationID OUTSIDE AccessLog so AccessLog
	// sees the stamped context. r.WithContext only propagates
	// inward through next.ServeHTTP, so the access log must run
	// inside the correlation-ID frame to read its context value.
	h := middleware.CorrelationID(middleware.AccessLog(&buf, nil, inner))

	req := httptest.NewRequest(http.MethodGet, "/decide/v2", nil)
	req.Header.Set(middleware.CorrelationIDHeader, "test-corr-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line accessLine
	_ = json.Unmarshal(buf.Bytes(), &line)
	if line.Attrs.Route != "/decide" {
		t.Errorf("attrs.route = %q, want /decide", line.Attrs.Route)
	}
	if line.Attrs.CorrelationID != "test-corr-123" {
		t.Errorf("attrs.correlation_id = %q, want test-corr-123", line.Attrs.CorrelationID)
	}
}

func TestAccessLogForwardsResponseBody(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rule":"enterprise","markup_factor":1.15}`))
	})
	h := middleware.AccessLog(&buf, nil, inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decide", nil))

	// statusRecorder.Write must forward the bytes; a regression that
	// dropped them would silently break every routed request.
	if rec.Body.String() != `{"rule":"enterprise","markup_factor":1.15}` {
		t.Errorf("response body = %q, want the inner's JSON forwarded", rec.Body.String())
	}
}

func TestAccessLogRouteEmptyWhenInnerDoesNotStamp(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Does NOT call SetMatchedRoute (e.g., the router missed).
		w.WriteHeader(http.StatusNotFound)
	})
	h := middleware.AccessLog(&buf, nil, inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))

	var line accessLine
	_ = json.Unmarshal(buf.Bytes(), &line)
	if line.Attrs.Route != "" {
		t.Errorf("attrs.route = %q on unmatched request, want empty", line.Attrs.Route)
	}
	if line.Attrs.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", line.Attrs.Status)
	}
}

func TestAccessLogDurationIsMeasured(t *testing.T) {
	var buf bytes.Buffer
	// Inject a deterministic clock: first call = start, second = +5ms.
	calls := 0
	now := func() time.Time {
		calls++
		t0 := time.Unix(1700000000, 0)
		if calls == 1 {
			return t0
		}
		return t0.Add(5 * time.Millisecond)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.AccessLog(&buf, now, inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide", nil))

	var line accessLine
	_ = json.Unmarshal(buf.Bytes(), &line)
	if line.Attrs.DurationMS != 5.0 {
		t.Errorf("duration_ms = %v, want 5.0", line.Attrs.DurationMS)
	}
}

func TestAccessLogConcurrentRequestsProduceWellFormedLines(t *testing.T) {
	var buf bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.AccessLog(&buf, nil, inner)

	const requests = 32
	var wg sync.WaitGroup
	wg.Add(requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/decide", nil))
		}()
	}
	wg.Wait()

	// Every line must decode cleanly; a torn write would produce
	// at least one decode failure.
	dec := json.NewDecoder(&buf)
	count := 0
	for {
		var line accessLine
		err := dec.Decode(&line)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode #%d: %v", count, err)
		}
		count++
	}
	if count != requests {
		t.Errorf("decoded %d lines, want %d (concurrent writes torn?)", count, requests)
	}
}

