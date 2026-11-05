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

func TestKeepRestartsAfterAPanicOnly(t *testing.T) {
	restartDelay = time.Millisecond
	var log bytes.Buffer
	calls := 0
	done := errors.New("done")
	err := Keep(context.Background(), slog.New(slog.NewTextHandler(&log, nil)), "test", func() error {
		if calls++; calls < 3 {
			panic("boom")
		}
		return done
	})
	if err != done || calls != 3 {
		t.Fatalf("err %v, calls %d", err, calls)
	}
	if !strings.Contains(log.String(), "service crashed") {
		t.Fatalf("log: %s", log.String())
	}
}

func TestKeepStopsWithItsContext(t *testing.T) {
	restartDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Keep(ctx, discard, "test", func() error { panic("boom") }); err != nil {
		t.Fatalf("err: %v", err)
	}
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
