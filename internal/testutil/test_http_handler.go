package testutil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// ResponseSpec describes how HTTP test server should reply to a single request.
type ResponseSpec struct {
	// StatusCode is the HTTP status code to return.
	StatusCode int

	// Body is the response body to return. If empty, no body will be returned.
	Body []byte

	// Active is set to true if the response should be used for the next request.
	Active *atomic.Bool
}

// The route spec describes how the HTTP test server should route and handle requests.
type RouteSpec struct {
	// Pattern (see net/http.ServeMux) to match the request path.
	Pattern string

	// Set of response specs to use for this route.
	// The first response that is "active" will be used.
	// if there is no active response spec, the request will return 404.
	Responses []ResponseSpec
}

// Start a new HTTP test server with the given route specs.
// The server will run until the lifetimeCtx is cancelled.
// Returns the URL of the server.
func ServeHttp(lifetimeCtx context.Context, routes []RouteSpec) string {
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.HandleFunc(route.Pattern, makeHandler(route.Responses))
	}

	server := httptest.NewServer(mux)
	go func() {
		<-lifetimeCtx.Done()
		server.Close()
	}()

	return server.URL
}

func makeHandler(responseSpecs []ResponseSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		i := slices.IndexFunc(responseSpecs, func(rs ResponseSpec) bool { return rs.Active.Load() })

		if i < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		spec := responseSpecs[i]
		w.WriteHeader(spec.StatusCode)
		if len(spec.Body) > 0 {
			_, err := w.Write(spec.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	}
}
