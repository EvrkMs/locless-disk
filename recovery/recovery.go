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

// restartDelay keeps a service that crashes at once from filling the log.
var restartDelay = time.Second

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

// Keep runs a service and starts it again after a panic, until ctx ends. An error
// it returns on its own ends it, as the caller decides what that means.
func Keep(ctx context.Context, log *slog.Logger, name string, serve func() error) error {
	for {
		err := Run(serve)
		stack := StackOf(err)
		if stack == "" {
			return err
		}
		log.Error("service crashed, restarting", "service", name, "err", err, "stack", stack)
		select {
		case <-ctx.Done():
			return nil
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
