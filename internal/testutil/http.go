// Package testutil provides in-process HTTP fixtures without opening ports.
package testutil

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type Server struct {
	URL     string
	restore func()
}

// NewServer routes this client's requests through handler, retaining http.Client
// request construction and the production response parser. Tests are sequential.
func NewServer(t *testing.T, client *http.Client, handler http.Handler) *Server {
	t.Helper()
	previous := client.Transport
	client.Transport = RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		response := recorder.Result()
		response.Request = r
		return response, nil
	})
	var once sync.Once
	s := &Server{URL: "http://127.0.0.1/v1", restore: func() { once.Do(func() { client.Transport = previous }) }}
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Close() { s.restore() }
