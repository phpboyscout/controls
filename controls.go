package controls

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

const (
	Stop   Message = "stop"
	Status Message = "status"
)

const (
	Unknown  State = "unknown"
	Running  State = "running"
	Stopping State = "stopping"
	Stopped  State = "stopped"
)

type State string
type Message string
type StartFunc func() error
type StopFunc func()
type StatusFunc func()
type ValidErrorFunc func(error) bool

type HealthMessage struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type Controllable interface {
	Messages() chan Message
	Health() chan HealthMessage
	Errors() chan error
	Signals() chan os.Signal
	SetErrorsChannel(errs chan error)
	SetMessageChannel(control chan Message)
	SetSignalsChannel(sigs chan os.Signal)
	SetHealthChannel(health chan HealthMessage)
	SetWaitGroup(wg *sync.WaitGroup)
	Start()
	Stop()
	GetContext() context.Context
	SetState(state State)
	GetState() State
	SetLogger(logger *slog.Logger)
	GetLogger() *slog.Logger
	IsRunning() bool
	IsStopped() bool
	IsStopping() bool
	Register(id string, start StartFunc, stop StopFunc, status StatusFunc)
}
