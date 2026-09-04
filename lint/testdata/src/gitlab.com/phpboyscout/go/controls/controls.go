// Package controls is a stub of the real module's surface, enough for the
// fixtures to type-check.
package controls

import "context"

type StartFunc func(context.Context) error

type StopFunc func(context.Context)

type ServiceOption func(*Service)

type Service struct {
	Name  string
	Start StartFunc
	Stop  StopFunc
}

func WithStart(fn StartFunc) ServiceOption { return func(s *Service) { s.Start = fn } }

func WithStop(fn StopFunc) ServiceOption { return func(s *Service) { s.Stop = fn } }

type Controller struct{}

func (c *Controller) Register(id string, opts ...ServiceOption) {}

type Supervisor struct{}

func NewSupervisor() *Supervisor { return &Supervisor{} }

func (s *Supervisor) Start(ctx context.Context) error { return nil }

func (s *Supervisor) Stop(ctx context.Context) {}

type Child struct {
	Name  string
	Start StartFunc
	Stop  StopFunc
}

func (s *Supervisor) Attach(c Child) error { return nil }
