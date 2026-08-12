package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestNewReqReusesHTTPClient(t *testing.T) {
	first := NewReq()
	second := NewReq()
	if first.client != second.client {
		t.Fatal("NewReq() did not reuse the shared HTTP client")
	}
}

func TestGetRejectsOversizedContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(maxTextResponseBytes+1, 10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	_, err := NewReq().Get(context.Background(), server.URL)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Get() error = %v, want ErrResponseTooLarge", err)
	}
}

func TestGetRejectsOversizedChunkedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		_, _ = w.Write(make([]byte, maxTextResponseBytes+1))
	}))
	t.Cleanup(server.Close)

	_, err := NewReq().Get(context.Background(), server.URL)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Get() error = %v, want ErrResponseTooLarge", err)
	}
}

func TestHTTPStatusErrorParsesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	_, err := NewReq().Get(context.Background(), server.URL)
	if HTTPStatusCode(err) != http.StatusTooManyRequests || HTTPRetryAfter(err) != 7*time.Second {
		t.Fatalf("status/retry-after = %d/%v", HTTPStatusCode(err), HTTPRetryAfter(err))
	}
}
