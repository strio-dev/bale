package acceptance_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// registryAPI keeps unrelated local HTTP probes outside registry observations.
// Every method under the OCI Distribution API is still observed and forwarded.
func registryAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2" && !strings.HasPrefix(r.URL.Path, "/v2/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestRegistryAPI(t *testing.T) {
	var backendRequests atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	upstream, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	requests, writes := 0, 0
	handler := registryAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes++
		}
		proxy.ServeHTTP(w, r)
	}))
	for _, test := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/v1/", http.StatusNotFound},
		{http.MethodGet, "/v20/", http.StatusNotFound},
		{http.MethodGet, "/v2", http.StatusNoContent},
		{http.MethodGet, "/v2/", http.StatusNoContent},
		{http.MethodHead, "/v2/example/manifests/latest", http.StatusNoContent},
		{http.MethodPost, "/v2/example/blobs/uploads/", http.StatusNoContent},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			beforeRequests, beforeWrites := requests, writes
			beforeBackend := backendRequests.Load()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.want {
				t.Errorf("status = %d, want %d", response.Code, test.want)
			}
			wantRequests, wantWrites := 0, 0
			if test.want != http.StatusNotFound {
				wantRequests = 1
				if test.method != http.MethodGet && test.method != http.MethodHead {
					wantWrites = 1
				}
			}
			if requests-beforeRequests != wantRequests || backendRequests.Load()-beforeBackend != int64(wantRequests) || writes-beforeWrites != wantWrites {
				t.Errorf("observed requests=%d backend=%d writes=%d, want requests=%d writes=%d", requests-beforeRequests, backendRequests.Load()-beforeBackend, writes-beforeWrites, wantRequests, wantWrites)
			}
		})
	}
	backend.Close()
	t.Run("stopped upstream", func(t *testing.T) {
		before := requests
		for _, test := range []struct {
			path string
			want int
		}{{"/", http.StatusNotFound}, {"/v2/", http.StatusBadGateway}} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != test.want {
				t.Errorf("%s status = %d, want %d", test.path, response.Code, test.want)
			}
		}
		if requests-before != 1 {
			t.Errorf("observed %d requests with stopped upstream, want only the OCI request", requests-before)
		}
	})
}
