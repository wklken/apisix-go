package server

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/wklken/apisix-go/pkg/etcd"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/store"
)

type Server struct {
	addr   string
	server *http.Server
}

func NewServer() (*Server, error) {
	return &Server{
		addr:   ":8080",
		server: &http.Server{},
	}, nil
}

func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		logger.Info("I have to go...")
		logger.Info("Stopping server gracefully")
		// TODO: rafactor the graceful shutdown
		s.server.Shutdown(ctx)
	}()

	prefix := "/apisix"
	endpoints := []string{"127.0.0.1:2379"}
	events := make(chan *store.Event)

	storage := store.NewStore("my.db", events)
	go storage.Start()

	etcdClient, err := etcd.NewConfigClient(endpoints, prefix, events)
	if err != nil {
		return err
	}
	etcdClient.FetchAll()
	go etcdClient.Watch()

	routes := storage.GetBucketData("routes")

	s.server.Handler = route.BuildRoute(routes)

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("error opening listener: %w", err)
	}

	return s.server.Serve(listener)
}
