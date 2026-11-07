// Package recovery keeps a crash in one piece of work from taking everything
// else down. Go has no try/catch: an error is a value that callers already handle,
// and only a panic, which is a bug, escapes that. A panic nobody recovers ends the
// whole process, so every goroutine that does real work runs through this package.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// restartDelay keeps a service that fails at once from taking a core.
var restartDelay = 5 * time.Second

// PanicError is a panic turned into an ordinary error.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Run calls f and returns its error, or a PanicError if it panicked.
func Run(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return f()
}

// StackOf returns the stack of a panic, or "" for any other error.
func StackOf(err error) string {
	var p *PanicError
	if errors.As(err, &p) {
		return string(p.Stack)
	}
	return ""
}

// quietAfter is how long a service has to run before the same failure is logged
// again: a port that stays taken must not write a line every few seconds.
const quietAfter = time.Minute

// Keep runs a service until ctx ends. Whenever it stops, on an error or a panic,
// it starts again after a pause: a port that was taken for a while or a network
// card that came back must not need a restart of the whole server.
func Keep(ctx context.Context, log *slog.Logger, name string, serve func() error) {
	last := ""
	for {
		started := time.Now()
		err := Run(serve)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stopped on its own")
		}
		if time.Since(started) > quietAfter {
			last = ""
		}
		if msg := err.Error(); msg != last {
			last = msg
			log.Error("service failed, restarting", "service", name, "err", err, "stack", StackOf(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartDelay):
		}
	}
}

// Handler logs a panic in a request and answers 500. net/http would survive it
// too, but only with a line on stderr, which a service does not have.
func Handler(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // how a handler drops a connection on purpose
			}
			log.Error("request crashed", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			http.Error(w, "server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
