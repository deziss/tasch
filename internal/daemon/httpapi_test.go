package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The CORS allow-list is what stands between this endpoint and any page a user happens to
// visit. It accepts job submissions, so an origin getting through here means arbitrary commands
// on the cluster with whatever token the browser already holds.
func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	handler := withCORS([]string{"https://tasch.example.com", "http://localhost:5173/"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	tests := []struct {
		origin string
		allow  bool
	}{
		{origin: "https://tasch.example.com", allow: true},
		// A configured origin with a trailing slash must still match what the browser sends,
		// which never has one.
		{origin: "http://localhost:5173", allow: true},
		{origin: "https://evil.example", allow: false},
		// Near-misses that a prefix or suffix comparison would wrongly admit.
		{origin: "https://tasch.example.com.evil.test", allow: false},
		{origin: "http://tasch.example.com", allow: false},
		{origin: "", allow: false},
	}

	for _, tc := range tests {
		t.Run(tc.origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/v1.SchedulerService/ListJobs", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			got := rec.Header().Get("Access-Control-Allow-Origin")
			if tc.allow && got != tc.origin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, tc.origin)
			}
			if !tc.allow && got != "" {
				t.Fatalf("origin %q was allowed; header = %q", tc.origin, got)
			}
		})
	}
}

// A cache that served one origin's allowance to another would defeat the allow-list entirely.
func TestCORSVariesOnOrigin(t *testing.T) {
	handler := withCORS([]string{"https://ui.example"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://ui.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("Vary = %q, want Origin", rec.Header().Get("Vary"))
	}
}

// A preflight must not reach the handler behind it.
func TestCORSPreflightShortCircuits(t *testing.T) {
	reached := false
	handler := withCORS([]string{"https://ui.example"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://ui.example")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if reached {
		t.Fatal("a preflight request was passed to the API handler")
	}
}

// An HTTP client has to see the same code a gRPC client would, or the two transports disagree
// about what happened — a permission failure showing up as a generic 500 is unactionable.
func TestConnectErrorPreservesCodes(t *testing.T) {
	tests := map[codes.Code]connect.Code{
		codes.PermissionDenied:  connect.CodePermissionDenied,
		codes.NotFound:          connect.CodeNotFound,
		codes.InvalidArgument:   connect.CodeInvalidArgument,
		codes.ResourceExhausted: connect.CodeResourceExhausted,
		codes.Unauthenticated:   connect.CodeUnauthenticated,
	}

	for grpcCode, want := range tests {
		err := connectError(status.Error(grpcCode, "denied"))
		if got := connect.CodeOf(err); got != want {
			t.Errorf("connectError(%v) = %v, want %v", grpcCode, got, want)
		}
	}

	if connectError(nil) != nil {
		t.Fatal("connectError(nil) should stay nil")
	}
}
