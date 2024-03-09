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

var dummyResource = []byte(`{
	"uri": "/get",
	"name": "dummy_get",
	"plugins": {},
	"service": {},
	"upstream": {
		"nodes": [
		{
			"host": "httpbin.org",
			"port": 80,
			"weight": 100
		}
		],
		"type": "roundrobin",
		"scheme": "http",
		"pass_host": "pass"
	}
}`)

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

	logger.Info("Starting storage")
	storage := store.NewStore("my.db", events)
	go storage.Start()

	logger.Info("Starting etcd client")
	etcdClient, err := etcd.NewConfigClient(endpoints, prefix, events)
	if err != nil {
		return err
	}
	logger.Info("fetch full data from etcd")
	err = etcdClient.FetchAll()
	if err != nil {
		panic(err)
	}
	logger.Info("watch etcd")
	go etcdClient.Watch()

	logger.Info("build the routes")
	routes := storage.GetBucketData("routes")
	routes = append(routes, dummyResource)
	s.server.Handler = route.BuildRoute(routes)

	logger.Info("server started")
	logger.Infof("listening on %s", s.addr)
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("error opening listener: %w", err)
	}

	return s.server.Serve(listener)
}
