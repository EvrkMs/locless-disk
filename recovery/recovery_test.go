package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunTurnsAPanicIntoAnError(t *testing.T) {
	want := errors.New("plain")
	if err := Run(func() error { return want }); err != want {
		t.Fatalf("plain error: %v", err)
	}
	if StackOf(want) != "" {
		t.Fatal("a plain error has no stack")
	}
	err := Run(func() error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(StackOf(err), "recovery_test.go") {
		t.Fatalf("stack: %q", StackOf(err))
	}
}

// TestKeepRestartsAfterAnyFailure: a plain error must not stop the server either,
// and the same failure goes to the log once.
func TestKeepRestartsAfterAnyFailure(t *testing.T) {
	restartDelay = time.Millisecond
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	Keep(ctx, slog.New(slog.NewTextHandler(&log, nil)), "test", func() error {
		switch calls++; calls {
		case 1:
			panic("boom")
		case 2, 3, 4:
			return errors.New("port is taken")
		}
		cancel()
		return nil
	})
	if calls != 5 {
		t.Fatalf("calls %d", calls)
	}
	if n := strings.Count(log.String(), "port is taken"); n != 1 || !strings.Contains(log.String(), "boom") {
		t.Fatalf("logged the error %d times: %s", n, log.String())
	}
}

func TestKeepStopsWithItsContext(t *testing.T) {
	restartDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	Keep(ctx, discard, "test", func() error { panic("boom") })
}

func TestHandlerAnswersAndLogs(t *testing.T) {
	var log bytes.Buffer
	h := Handler(slog.New(slog.NewTextHandler(&log, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(log.String(), "request crashed") {
		t.Fatalf("code %d, log %s", w.Code, log.String())
	}
}
