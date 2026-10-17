package controls_test

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// logLine is one record with its attributes, those added by With included.
type logLine struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// attrLog records every line with its attributes. Unlike levelLog it honours
// WithAttrs, which WithSupervisorLogger's component attribute goes through.
type attrLog struct {
	mu    *sync.Mutex
	lines *[]logLine
	with  []slog.Attr
}

func newAttrLog() attrLog { return attrLog{mu: &sync.Mutex{}, lines: &[]logLine{}} }

func (l attrLog) Enabled(context.Context, slog.Level) bool { return true }
func (l attrLog) WithGroup(string) slog.Handler            { return l }

func (l attrLog) WithAttrs(as []slog.Attr) slog.Handler {
	l.with = append(append([]slog.Attr(nil), l.with...), as...)

	return l
}

func (l attrLog) Handle(_ context.Context, r slog.Record) error {
	line := logLine{level: r.Level, msg: r.Message, attrs: map[string]string{}}

	for _, a := range l.with {
		line.attrs[a.Key] = a.Value.String()
	}

	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()

		return true
	})

	l.mu.Lock()
	defer l.mu.Unlock()

	*l.lines = append(*l.lines, line)

	return nil
}

// first returns the first line logged with msg.
func (l attrLog) first(msg string) (logLine, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range *l.lines {
		if line.msg == msg {
			return line, true
		}
	}

	return logLine{}, false
}

// requireLine waits for a line logged with msg and returns it.
func requireLine(t *testing.T, logs attrLog, msg string) logLine {
	t.Helper()

	var line logLine

	require.Eventually(t, func() bool {
		var ok bool
		line, ok = logs.first(msg)

		return ok
	}, 3*time.Second, 5*time.Millisecond, "nothing logged %q", msg)

	return line
}

// requireCallbackPanicLine checks the attributes every callback-panic line
// carries, and that its stack names the test whose callback panicked.
func requireCallbackPanicLine(t *testing.T, line logLine, want map[string]string) {
	t.Helper()

	require.Equal(t, slog.LevelError, line.level)
	require.Equal(t, "callback", line.attrs["panic"])
	test, _, _ := strings.Cut(t.Name(), "/")
	require.Contains(t, line.attrs["stack"], test+".func", "the stack does not reach the callback that panicked")

	for k, v := range want {
		require.Equal(t, v, line.attrs[k], "attribute %s", k)
	}
}

func panicsInStart(context.Context) error { panic("start") }

func TestSupervisorLogsAPanickingOnFailureCallback(t *testing.T) {
	t.Parallel()

	logs := newAttrLog()

	var restarts atomic.Int64

	s := controls.NewSupervisor(
		controls.WithSupervisorLogger(slog.New(logs)),
		controls.WithOnFailure(func(f controls.Failure) {
			restarts.Store(int64(f.Restarts))
			panic("callback")
		}))

	_ = s.Attach(controls.Child{Name: "worker", Start: alwaysFails})
	_ = s.Start(t.Context())

	line := requireLine(t, logs, "WithOnFailure callback panicked")
	requireCallbackPanicLine(t, line, map[string]string{
		"component": "supervisor",
		"child":     "worker",
		"restarts":  strconv.FormatInt(restarts.Load(), 10),
	})

	s.Stop(t.Context())
}

func TestSupervisorLogsAPanickingOnChildPanicCallback(t *testing.T) {
	t.Parallel()

	logs := newAttrLog()

	s := controls.NewSupervisor(
		controls.WithSupervisorLogger(slog.New(logs)),
		controls.WithOnChildPanic(func(controls.PanicReport) { panic("callback") }))

	_ = s.Attach(controls.Child{Name: "worker", Start: panicsInStart})
	_ = s.Start(t.Context())

	line := requireLine(t, logs, "WithOnChildPanic callback panicked")
	requireCallbackPanicLine(t, line, map[string]string{
		"component": "supervisor",
		"child":     "worker",
		"site":      "start",
	})

	s.Stop(t.Context())
}

// TestSupervisorAcceptsANilLogger — nil discards, like the default, and a
// panicking callback is still survived.
func TestSupervisorAcceptsANilLogger(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	s := controls.NewSupervisor(
		controls.WithSupervisorLogger(nil),
		controls.WithOnFailure(func(controls.Failure) {
			calls.Add(1)
			panic("callback")
		}))

	_ = s.Attach(controls.Child{Name: "a", Start: alwaysFails})
	_ = s.Attach(controls.Child{Name: "b", Start: alwaysFails})
	_ = s.Start(t.Context())

	require.Eventually(t, func() bool { return calls.Load() == 2 }, 3*time.Second, 5*time.Millisecond,
		"a panicking callback stopped later deliveries")

	s.Stop(t.Context())
}

// TestAPanickingHandlerCannotCrashASupervisorCallbackPanicLog is spec 0011 D5:
// the line is logged inside a deferred recover, so a handler that panics there
// has nothing left to recover it.
func TestAPanickingHandlerCannotCrashASupervisorCallbackPanicLog(t *testing.T) {
	t.Parallel()

	callbacks := []struct {
		name  string
		start controls.StartFunc
		opt   func(calls *atomic.Int64) controls.SupervisorOption
	}{
		{
			name:  "WithOnFailure",
			start: alwaysFails,
			opt: func(calls *atomic.Int64) controls.SupervisorOption {
				return controls.WithOnFailure(func(controls.Failure) {
					calls.Add(1)
					panic("callback")
				})
			},
		},
		{
			name:  "WithOnChildPanic",
			start: panicsInStart,
			opt: func(calls *atomic.Int64) controls.SupervisorOption {
				return controls.WithOnChildPanic(func(controls.PanicReport) {
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

				s := controls.NewSupervisor(
					controls.WithSupervisorLogger(slog.New(panickyHandler{inEnabled: inEnabled, panics: &panics})),
					cb.opt(&calls))

				_ = s.Attach(controls.Child{Name: "a", Start: cb.start})
				_ = s.Attach(controls.Child{Name: "b", Start: cb.start})
				_ = s.Start(t.Context())

				require.Eventually(t, func() bool { return calls.Load() >= 2 }, 3*time.Second, 5*time.Millisecond,
					"the callback stopped receiving after the handler panicked")
				require.Positive(t, panics.Load(), "the handler never panicked, so nothing was tested")

				stopped := make(chan struct{})

				go func() {
					s.Stop(t.Context())
					close(stopped)
				}()

				select {
				case <-stopped:
				case <-time.After(2 * time.Second):
					t.Fatal("the supervisor did not stop")
				}
			})
		}
	}
}

// TestControllerCallbackPanicLinesCarryTheStack is spec 0011 D6: the
// controller's two lines read like the Supervisor's.
func TestControllerCallbackPanicLinesCarryTheStack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		msg   string
		start controls.StartFunc
		opt   controls.ControllerOpt
		want  map[string]string
	}{
		{
			msg:   "WithOnEvent callback panicked",
			start: alwaysFails,
			opt:   controls.WithOnEvent(func(controls.ServiceEvent) { panic("callback") }),
			want:  map[string]string{"component": "controller", "service_name": "svc"},
		},
		{
			msg:   "WithOnPanic callback panicked",
			start: panicsInStart,
			opt:   controls.WithOnPanic(func(controls.PanicReport) { panic("callback") }),
			want:  map[string]string{"component": "controller", "service_name": "svc", "site": "start"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			t.Parallel()

			logs := newAttrLog()
			c := controls.NewController(context.Background(), controls.WithLogger(slog.New(logs)), tt.opt)

			c.Register("svc", controls.WithStart(tt.start))
			c.Start()

			line := requireLine(t, logs, tt.msg)
			requireCallbackPanicLine(t, line, tt.want)

			c.Stop()
			requireDone(t, c, 2*time.Second)
		})
	}
}
