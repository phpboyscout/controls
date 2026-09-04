// Package grpc is a stub of the one type the fixtures need.
package grpc

import "net"

type Server struct{}

func NewServer() *Server { return &Server{} }

func (s *Server) Serve(lis net.Listener) error { return nil }

func (s *Server) GracefulStop() {}
