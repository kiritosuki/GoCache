package goch

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/kiritosuki/GoCache/pb"
	"github.com/kiritosuki/GoCache/register"
	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	DefaultServerDialTimeout = 5 * time.Second
	DefaultServerMaxMsgSize  = 4 << 20 // 4MB 左移一位就是 x2
)

// Server 缓存服务器
// 实现grpc中的server接口
type Server struct {
	pb.UnimplementedGoCacheServer
	addr        string            // 服务地址
	serviceName string            // 服务名称
	mu          sync.Mutex        // 互斥锁
	groups      map[string]*Group // 缓存组
	grpcServer  *grpc.Server      // grpc服务器
	etcdCli     *clientv3.Client  // etcd客户端
	stopCh      chan error        // 停止信号
	opts        *ServerOptions    // 服务器选项
}

// ServerOptions 缓存服务器配置选项
type ServerOptions struct {
	EtcdEndpoints []string
	DialTimeout   time.Duration
	MaxMsgSize    int    // 最大消息大小
	TLS           bool   // 是否启用TLS
	CertFile      string // 证书文件
	KeyFile       string // 密钥文件
}

// DefaultServerOptions 缓存服务器默认配置
var DefaultServerOptions = &ServerOptions{
	EtcdEndpoints: []string{"localhost:2379"},
	DialTimeout:   DefaultServerDialTimeout,
	MaxMsgSize:    DefaultServerMaxMsgSize,
}

// ServerOption 函数式选项 配置缓存服务器的可选配置
type ServerOption func(opts *ServerOptions)

// WithEtcdEndpoints 设置etcd的endpoints
func WithEtcdEndpoints(endpoints []string) ServerOption {
	return func(opts *ServerOptions) {
		opts.EtcdEndpoints = endpoints
	}
}

// WithDialTimeout 设置超时连接时长
func WithDialTimeout(timeout time.Duration) ServerOption {
	return func(opts *ServerOptions) {
		opts.DialTimeout = timeout
	}
}

// WithTLS TLS配置
func WithTLS(certFile string, keyFile string) ServerOption {
	return func(opts *ServerOptions) {
		opts.TLS = true
		opts.CertFile = certFile
		opts.KeyFile = keyFile
	}
}

// NewServer 创建缓存服务器实例
func NewServer(addr string, serviceName string, opts ...ServerOption) (*Server, error) {
	options := *DefaultServerOptions
	for _, opt := range opts {
		opt(&options)
	}
	// 创建etcd客户端
	etcdCli, err := clientv3.New(clientv3.Config{
		Endpoints:   options.EtcdEndpoints,
		DialTimeout: options.DialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create etcd client: %w", err)
	}
	// 处理grpc选项 信息最大长度和TLS证书
	// 建立grpc连接要用 所以从自己的业务options中抽离出来
	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxRecvMsgSize(options.MaxMsgSize))
	if options.TLS {
		// 加载TLS证书
		creds, err := loadTLSCredentials(options.CertFile, options.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS credentials: %w", err)
		}
		grpcOptions = append(grpcOptions, grpc.Creds(creds))
	}

	// 创建grpc服务端操作对象
	grpcServer := grpc.NewServer(grpcOptions...)

	server := &Server{
		addr:        addr,
		serviceName: serviceName,
		groups:      make(map[string]*Group),
		grpcServer:  grpcServer,
		etcdCli:     etcdCli,
		stopCh:      make(chan error),
		opts:        &options,
	}

	// 注册服务
	pb.RegisterGoCacheServer(grpcServer, server)
	// 注册健康检查服务
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	// 标记当前服务是 Serving 状态 即健康的
	healthServer.SetServingStatus(serviceName, healthpb.HealthCheckResponse_SERVING)
	return server, err
}

// Start 启动服务器
func (s *Server) Start() error {
	// grpc服务端监听
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	// 注册到etcd
	stopCh := make(chan error)
	go func() {
		if err := register.Register(s.serviceName, s.addr, stopCh); err != nil {
			logrus.Errorf("failed to register service: %v", err)
			close(stopCh)
			return
		}
	}()
	logrus.Infof("server starting at %s", s.addr)
	// 阻塞监听服务端的请求 会自己开协程处理这些请求
	return s.grpcServer.Serve(listener)
}

/* 实现 GoCacheServer 的接口 */

// Get 实现缓存服务的Get接口
func (s *Server) Get(ctx context.Context, req *pb.Request) (*pb.ResponseForGet, error) {
	group := GetGroup(req.Group)
	if group == nil {
		return nil, fmt.Errorf("group %s not found", req.Group)
	}
	view, err := group.Get(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	return &pb.ResponseForGet{Value: view.GetBytes()}, nil
}

// Put 实现缓存服务的Put方法
func (s *Server) Put(ctx context.Context, req *pb.Request) (*pb.ResponseForGet, error) {
	group := GetGroup(req.Group)
	if group == nil {
		return nil, fmt.Errorf("group %s not found", req.Group)
	}
	// 从context中获取标记 如果没有则创建新的context
	// 这里是因为 Server的方法是通过grpc暴露的
	// 也就是这里的Server接口实际上是给peer远程调用使用的 而不是用户调用
	// 所以其他peer会调用该节点的Put 一定是说明在同步请求
	// 该节点只需要执行同步来的请求 不能二次传播
	// 这里做兜底机制 尽管ctx中之前已经设置了from_peer=true 这里再设置一遍
	fromPeer := ctx.Value("from_peer")
	if fromPeer == nil {
		ctx = context.WithValue(ctx, "from_peer", true)
	}
	if err := group.Put(ctx, req.Key, req.Value); err != nil {
		return nil, err
	}
	return &pb.ResponseForGet{Value: req.Value}, nil
}

// Delete 实现缓存服务的Delete方法
func (s *Server) Delete(ctx context.Context, req *pb.Request) (*pb.ResponseForDelete, error) {
	group := GetGroup(req.Group)
	if group == nil {
		return nil, fmt.Errorf("group %s not found", req.Group)
	}
	if ctx.Value("from_peer") == nil {
		ctx = context.WithValue(ctx, "from_peer", true)
	}
	err := group.Delete(ctx, req.Key)
	return &pb.ResponseForDelete{Value: err == nil}, err
}

/* 辅助函数 */

// loadTLSCredentials 加载TLS证书
func loadTLSCredentials(certFile string, keyFile string) (credentials.TransportCredentials, error) {
	// 加载公共证书和私钥 获取凭证
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
	}), nil
}
