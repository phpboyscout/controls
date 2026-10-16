package controls_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// panickyHandler panics while logging a callback's panic, in Handle or, with
// inEnabled, in Enabled, and counts each time it does.
type panickyHandler struct {
	inEnabled bool
	panics    *atomic.Int64
}

func (h panickyHandler) Enabled(context.Context, slog.Level) bool {
	// Enabled cannot see the message, so it finds the callback-panic lines by
	// their caller. The panic count proves the name still matches.
	if h.inEnabled {
		stack := string(debug.Stack())
		if strings.Contains(stack, "invokeOnEvent") || strings.Contains(stack, "invokeOnPanic") {
			h.panics.Add(1)
			panic("handler: Enabled")
		}
	}

	return true
}

func (h panickyHandler) Handle(_ context.Context, r slog.Record) error {
	if !h.inEnabled && strings.HasSuffix(r.Message, "callback panicked") {
		h.panics.Add(1)
		panic("handler: Handle")
	}

	return nil
}

func (h panickyHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h panickyHandler) WithGroup(string) slog.Handler      { return h }

// TestAPanickingHandlerCannotCrashACallbackPanicLog is issue 23: the callback's
// panic is logged inside a deferred recover, so a handler that panics there has
// nothing left to recover it.
func TestAPanickingHandlerCannotCrashACallbackPanicLog(t *testing.T) {
	t.Parallel()

	failing := func(context.Context) error { return errors.New("nope") }
	panicking := func(context.Context) error { panic("start") }

	callbacks := []struct {
		name  string
		start controls.StartFunc
		opt   func(calls *atomic.Int64) controls.ControllerOpt
	}{
		{
			name:  "WithOnEvent",
			start: failing,
			opt: func(calls *atomic.Int64) controls.ControllerOpt {
				return controls.WithOnEvent(func(controls.ServiceEvent) {
					calls.Add(1)
					panic("callback")
				})
			},
		},
		{
			name:  "WithOnPanic",
			start: panicking,
			opt: func(calls *atomic.Int64) controls.ControllerOpt {
				return controls.WithOnPanic(func(controls.PanicReport) {
					calls.Add(1)
					panic("callback")
				})
			},
		},
	}

	for _, cb := range callbacks {
		for _, inEnabled := range []bool{false, true} {
			name := cb.name + "/Handle"
			if inEnabled {
				name = cb.name + "/Enabled"
			}

			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var calls, panics atomic.Int64

				c := controls.NewController(context.Background(),
					controls.WithLogger(slog.New(panickyHandler{inEnabled: inEnabled, panics: &panics})),
					cb.opt(&calls))

				c.Register("a", controls.WithStart(cb.start))
				c.Register("b", controls.WithStart(cb.start))
				c.Start()

				require.Eventually(t, func() bool { return calls.Load() == 2 }, 3*time.Second, 5*time.Millisecond,
					"the callback stopped receiving after the handler panicked")
				require.Positive(t, panics.Load(), "the handler never panicked, so nothing was tested")

				c.Stop()
				requireDone(t, c, 2*time.Second)
			})
		}
	}
}
