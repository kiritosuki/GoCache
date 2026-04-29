package goch

import (
	"context"
	"fmt"
	"time"

	"github.com/kiritosuki/GoCache/pb"
	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	DefaultEtcdDialTimeout = 5 * time.Second
	GrpcContextTimeout     = 3 * time.Second
)

// Client 缓存服务客户端
type Client struct {
	addr        string
	serviceName string
	etcdCli     *clientv3.Client
	conn        *grpc.ClientConn
	grpcCli     pb.GoCacheClient
}

// 类似 var a int = 1
// 这里把*Client类型的变量赋值给一个Peer类型的变量 且变量名为_
// 目的是在编译期检查 *Client 是否实现了 Peer 接口
var _ Peer = (*Client)(nil)

// NewClient 创建Client对象
// grpc中client接口无需实现 已有默认实现
func NewClient(addr string, serviceName string, etcdCli *clientv3.Client) (*Client, error) {
	var err error
	if etcdCli == nil {
		// 创建etcd客户端
		etcdCli, err = clientv3.New(clientv3.Config{
			Endpoints:   []string{"localhost:2379"},
			DialTimeout: DefaultEtcdDialTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create etcd client: %w", err)
		}
	}
	// 建立grpc客户端连接
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial server: %v", err)
	}
	// 创建grpc客户端操作对象
	grpcClient := pb.NewGoCacheClient(conn)
	client := &Client{
		addr:        addr,
		serviceName: serviceName,
		etcdCli:     etcdCli,
		conn:        conn,
		grpcCli:     grpcClient,
	}
	return client, nil
}

func (c *Client) Get(group string, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), GrpcContextTimeout)
	defer cancel()
	resp, err := c.grpcCli.Get(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get value from GoCache: %v", err)
	}
	return resp.GetValue(), nil
}

func (c *Client) Delete(group string, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), GrpcContextTimeout)
	defer cancel()
	resp, err := c.grpcCli.Delete(ctx, &pb.Request{
		Group: group,
		Key:   key,
	})
	if err != nil {
		return false, fmt.Errorf("failed to delete value from GoCache: %v", err)
	}
	return resp.GetValue(), nil
}

func (c *Client) Put(ctx context.Context, group string, key string, value []byte) error {
	resp, err := c.grpcCli.Put(ctx, &pb.Request{
		Group: group,
		Key:   key,
		Value: value,
	})
	if err != nil {
		return fmt.Errorf("failed to put value to GoCache: %v", err)
	}
	logrus.Infof("grpc put request resp: %+v", resp)
	return nil
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
