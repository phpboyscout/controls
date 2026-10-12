package controls_test

import (
	"context"
	"log/slog"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.com/phpboyscout/go/controls"
)

// levelLog records every line, so a test can ask what was logged at which level.
type levelLog struct {
	mu    sync.Mutex
	lines []slog.Record
}

func (l *levelLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *levelLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *levelLog) WithGroup(string) slog.Handler            { return l }

func (l *levelLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lines = append(l.lines, r.Clone())

	return nil
}

func (l *levelLog) records() []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]slog.Record(nil), l.lines...)
}

// TestACleanShutdownLogsNothingAtWarn is issue 21: an operator reads a warning
// as a fault, so a normal quit, whatever starts it, must not log one.
func TestACleanShutdownLogsNothingAtWarn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []controls.ControllerOpt
		svc     []controls.ServiceOption
		trigger func(c *controls.Controller, cancelParent func())
		want    controls.StopCause
	}{
		{
			name:    "Stop",
			trigger: func(c *controls.Controller, _ func()) { c.Stop() },
			want:    controls.CauseStop,
		},
		{
			name:    "parent cancelled",
			trigger: func(_ *controls.Controller, cancelParent func()) { cancelParent() },
			want:    controls.CauseParent,
		},
		{
			name:    "signal",
			opts:    []controls.ControllerOpt{controls.WithSignals()},
			trigger: func(c *controls.Controller, _ func()) { c.Signals() <- syscall.SIGINT },
			want:    controls.CauseSignal,
		},
		{
			name: "completed",
			svc: []controls.ServiceOption{
				controls.WithStart(func(context.Context) error { return nil }),
				controls.WithRunToCompletion(),
			},
			trigger: func(*controls.Controller, func()) {},
			want:    controls.CauseCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent, cancelParent := context.WithCancel(context.Background())
			t.Cleanup(cancelParent)

			logs := &levelLog{}
			c := controls.NewController(parent,
				append([]controls.ControllerOpt{controls.WithLogger(slog.New(logs))}, tt.opts...)...)

			svc := tt.svc
			if svc == nil {
				svc = []controls.ServiceOption{controls.WithStart(blocksUntilDone)}
			}

			c.Register("svc", svc...)
			c.Start()

			tt.trigger(c, cancelParent)
			requireDone(t, c, 2*time.Second)

			var sawStopping bool

			for _, r := range logs.records() {
				require.Less(t, r.Level, slog.LevelWarn, "a clean shutdown logged %q at %s", r.Message, r.Level)

				if r.Message == "Stopping Services" {
					sawStopping = true

					var cause string

					r.Attrs(func(a slog.Attr) bool {
						if a.Key == "cause" {
							cause = a.Value.String()
						}

						return true
					})

					require.Equal(t, string(tt.want), cause, "Stopping Services names the wrong cause")
				}
			}

			require.True(t, sawStopping, "the shutdown never logged Stopping Services")
		})
	}
}
