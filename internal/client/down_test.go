package client

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// A daemon whose own health check reports its loop stalled (503) is not
// usable: Dial keeps waiting and then fails, so the wrapper fails open
// instead of queueing on a wedged daemon.
func TestDialTreatsServerErrorAsDown(t *testing.T) {
	sock := filepath.Join(shortDir(t), "503.sock")
	serve(t, sock, func(s *server) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("loop stalled: last step 12000ms ago\n"))
		})
	})
	start := time.Now()
	c, err := Dial(context.Background(), sock, 200*time.Millisecond)
	if err == nil || c != nil {
		t.Fatalf("Dial = %v, %v; want an error for a stalled daemon", c, err)
	}
	if !IsStatus(err, 503) || !IsDown(err) {
		t.Fatalf("err %v; want the 503 APIError", err)
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("Dial gave up after %v; want ~the whole wait", el)
	}
}

func TestIsDown(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&APIError{Status: 404}, false},
		{&APIError{Status: 400}, false},
		{&APIError{Status: 500}, true},
		{&APIError{Status: 503}, true},
		{&DecodeError{Line: "x"}, false},
		{context.DeadlineExceeded, true},
	} {
		if got := IsDown(tc.err); got != tc.want {
			t.Errorf("IsDown(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
