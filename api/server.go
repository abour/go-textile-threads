package api

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/improbable-eng/grpc-web/go/grpcweb"
	logging "github.com/ipfs/go-log"
	ma "github.com/multiformats/go-multiaddr"
	tserv "github.com/textileio/go-textile-core/threadservice"
	pb "github.com/textileio/go-textile-threads/api/pb"
	es "github.com/textileio/go-textile-threads/eventstore"
	"github.com/textileio/go-textile-threads/util"
	logger "github.com/whyrusleeping/go-logging"
	"google.golang.org/grpc"
)

var (
	log = logging.Logger("api")
)

// Server provides a gRPC API to a store manager.
type Server struct {
	rpc     *grpc.Server
	proxy   *http.Server
	service *service

	ctx    context.Context
	cancel context.CancelFunc
}

// Config specifies server settings.
type Config struct {
	RepoPath  string
	Addr      ma.Multiaddr
	ProxyAddr ma.Multiaddr
	Debug     bool
}

// NewServer starts and returns a new server with the given threadservice.
// The threadservice is *not* managed by the server.
func NewServer(ctx context.Context, ts tserv.Threadservice, conf Config) (*Server, error) {
	var err error
	if conf.Debug {
		err = util.SetLogLevels(map[string]logger.Level{
			"api": logger.DEBUG,
		})
		if err != nil {
			return nil, err
		}
	}

	manager, err := es.NewManager(
		ts,
		es.WithJsonMode(true),
		es.WithRepoPath(conf.RepoPath),
		es.WithDebug(true))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	s := &Server{
		rpc:     grpc.NewServer(),
		service: &service{manager: manager},
		ctx:     ctx,
		cancel:  cancel,
	}

	addr, err := util.TCPAddrFromMultiAddr(conf.Addr)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		pb.RegisterAPIServer(s.rpc, s.service)
		s.rpc.Serve(listener)
	}()

	webrpc := grpcweb.WrapServer(
		s.rpc,
		grpcweb.WithOriginFunc(func(origin string) bool {
			return true
		}),
		grpcweb.WithWebsockets(true),
		grpcweb.WithWebsocketOriginFunc(func(req *http.Request) bool {
			return true
		}))
	proxyAddr, err := util.TCPAddrFromMultiAddr(conf.ProxyAddr)
	if err != nil {
		return nil, err
	}
	s.proxy = &http.Server{
		Addr: proxyAddr,
	}
	s.proxy.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if webrpc.IsGrpcWebRequest(r) ||
			webrpc.IsAcceptableGrpcCorsRequest(r) ||
			webrpc.IsGrpcWebSocketRequest(r) {
			webrpc.ServeHTTP(w, r)
		}
	})

	errc := make(chan error)
	go func() {
		errc <- s.proxy.ListenAndServe()
		close(errc)
	}()
	go func() {
		for err := range errc {
			if err != nil {
				if err == http.ErrServerClosed {
					break
				} else {
					log.Errorf("proxy error: %s", err)
				}
			}
		}
		log.Info("proxy was shutdown")
	}()

	return s, nil
}

// Close the server and the store manager.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.proxy.Shutdown(ctx); err != nil {
		log.Errorf("error shutting down proxy: %s", err)
	}

	s.rpc.GracefulStop()
	s.service.manager.Close()
	s.cancel()
}
