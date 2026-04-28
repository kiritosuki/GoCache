package register

import (
	"context"
	"fmt"
	"net"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	DefaultEtcdDialTimeout = 5 * time.Second
	DefaultEtcdLeaseTime   = 10 // 单位: s
)

// Config 定义etcd服务端配置
type Config struct {
	EndPoints   []string      // etcd集群地址
	DialTimeout time.Duration // 连接超时时间
}

// DefaultConfig 提供etcd服务端默认配置
var DefaultConfig = &Config{
	EndPoints:   []string{"localhost:2379"},
	DialTimeout: DefaultEtcdDialTimeout,
}

// Register 注册服务到etcd
func Register(svcName string, addr string, stopCh <-chan error) error {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   DefaultConfig.EndPoints,
		DialTimeout: DefaultConfig.DialTimeout,
	})
	if err != nil {
		return fmt.Errorf("failed to create etcd client: %v", err)
	}
	localIp, err := getLocalIp()
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to get local IP: %v", err)
	}
	if addr[0] == ':' {
		addr = fmt.Sprintf("%s%s", localIp, addr)
	}
	// 创建租约
	lease, err := cli.Grant(context.Background(), DefaultEtcdLeaseTime)
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to create lease: %v", err)
	}
	// 注册服务
	// key格式：/services/service_name/ip:port
	// value存储ip:port
	key := fmt.Sprintf("/services/%s/%s", svcName, addr)
	_, err = cli.Put(context.Background(), key, addr, clientv3.WithLease(lease.ID))
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to put key-value to etcd: %v", err)
	}
	// 保持租约
	keepAliveCh, err := cli.KeepAlive(context.Background(), lease.ID)
	if err != nil {
		cli.Close()
		return fmt.Errorf("failed to keep lease alive: %v", err)
	}
	// 处理租约续期和服务注销
	go func() {
		defer cli.Close()
		for {
			select {
			case <-stopCh:
				// 服务注销 撤销租约
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				cli.Revoke(ctx, lease.ID)
				cancel()
				return
			case
			}
		}
	}()
}

/* 辅助函数 */
// getLocalIp 获取当前设备的家庭局域网/公网ip地址
// TODO 后续可能要优化获取ip地址的方式 目前是找到第一个符合条件的ipv4地址 可能会受虚拟机/代理干扰
func getLocalIp() (string, error) {
	// 获取机器上所有网卡的地址
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		// 仅提取ip地址 且剔除localhost
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			// 只要ipv4
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no valid local IP found")
}
