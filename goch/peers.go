package goch

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kiritosuki/GoCache/register"
	"github.com/kiritosuki/GoCache/utils/consistenthash"
	"github.com/sirupsen/logrus"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	DefaultServiceName  = "go-cache"
	FetchServiceTimeout = 3 * time.Second
)

// PeerPicker 定义了peer选择器的接口
type PeerPicker interface {
	PickPeer(key string) (peer Peer, ok bool, isSelf bool)
	Close() error
}

// Peer 定义了缓存节点的接口
type Peer interface {
	Get(group string, key string) ([]byte, error)
	Put(ctx context.Context, group string, key string, value []byte) error
	Delete(group string, key string) (bool, error)
	Close() error
}

// ClientPicker 该结构体实现了 PeerPicker 接口
type ClientPicker struct {
	selfAddr    string
	serviceName string
	mu          sync.Mutex
	consHash    *consistenthash.Map
	clients     map[string]*Client
	etcdCli     *clientv3.Client
	ctx         context.Context
	cancel      context.CancelFunc
}

// PickerOption 函数式接口 定义配置选项
type PickerOption func(p *ClientPicker)

// WithServiceName 设置服务名称
func WithServiceName(name string) PickerOption {
	return func(p *ClientPicker) {
		p.serviceName = name
	}
}

// PrintPeers 打印当前已经发现的节点(仅用于测试)
func (p *ClientPicker) PrintPeers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	log.Printf("当前已发现的节点:")
	for addr := range p.clients {
		log.Printf("- %s", addr)
	}
}

// NewClientPicker 创建ClientPicker实例
func NewClientPicker(addr string, opts ...PickerOption) (*ClientPicker, error) {
	ctx, cancel := context.WithCancel(context.Background())
	picker := &ClientPicker{
		selfAddr:    addr,
		serviceName: DefaultServiceName,
		consHash:    consistenthash.New(),
		clients:     make(map[string]*Client),
		ctx:         ctx,
		cancel:      cancel,
	}
	for _, opt := range opts {
		opt(picker)
	}
	// 当前节点也必须在一致性哈希环上。PickPeer 命中自身时返回 isSelf=true，
	// Group 会直接使用本地 getter，避免节点之间在缓存未命中时互相转发。
	if err := picker.consHash.Add(addr); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to add self to consistent hash: %w", err)
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   register.DefaultConfig.EndPoints,
		DialTimeout: register.DefaultConfig.DialTimeout,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create etcd client: %w", err)
	}
	picker.etcdCli = cli
	// 启动服务发现
	if err := picker.startServiceDiscovery(); err != nil {
		cancel()
		cli.Close()
		return nil, err
	}
	return picker, nil
}

// startServiceDiscovery 启动服务发现
func (p *ClientPicker) startServiceDiscovery() error {
	// 先进行全量更新 发现现有的所有服务
	if err := p.fetchAllServices(); err != nil {
		return err
	}
	// 后台常驻监听服务更新 及时更新服务变化
	go p.watchServiceChanges()
	return nil
}

// PickPeer 选择peer节点
// 返回值: peer, ok ,isSelf
func (p *ClientPicker) PickPeer(key string) (Peer, bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 找到key对应的真实节点addr
	if addr := p.consHash.Get(key); addr != "" {
		if addr == p.selfAddr {
			return nil, true, true
		}
		if client, ok := p.clients[addr]; ok {
			// 根据addr映射找到服务实例client
			return client, true, false
		}
	}
	return nil, false, false
}

// Close 关闭所有资源
func (p *ClientPicker) Close() error {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	for addr, client := range p.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("failed to close client %s: %w", addr, err))
		}
	}
	if err := p.etcdCli.Close(); err != nil {
		errs = append(errs, fmt.Errorf("failed to close etcd client: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors while closing: %v", errs)
	}
	return nil
}

// fetchAllServices 发现所有服务实例
func (p *ClientPicker) fetchAllServices() error {
	ctx, cancel := context.WithTimeout(p.ctx, FetchServiceTimeout)
	defer cancel()
	// 服务发现需要用前缀匹配 /services/service_name
	resp, err := p.etcdCli.Get(ctx, "/services/"+p.serviceName, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get all services: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, kv := range resp.Kvs {
		addr := string(kv.Value)
		if addr != "" && addr != p.selfAddr {
			// 添加发现的服务实例
			p.put(addr)
			logrus.Infof("discovered service at %s", addr)
		}
	}
	return nil
}

// put 添加服务实例
func (p *ClientPicker) put(addr string) {
	client, err := NewClient(addr, p.serviceName, p.etcdCli)
	if err != nil {
		logrus.Errorf("failed to create client for %s, %v", addr, err)
		return
	}
	// 添加节点
	p.consHash.Add(addr)
	// 添加记录
	p.clients[addr] = client
	logrus.Infof("successfully created client for %s", addr)
}

// remove 移除服务实例
func (p *ClientPicker) remove(addr string) {
	// 移除节点
	p.consHash.Remove(addr)
	// 移除记录
	delete(p.clients, addr)
}

// watchServiceChanges 监听服务实例的变化
func (p *ClientPicker) watchServiceChanges() {
	watcher := clientv3.NewWatcher(p.etcdCli)
	watchChan := watcher.Watch(p.ctx, "/services/"+p.serviceName, clientv3.WithPrefix())
	for {
		select {
		case <-p.ctx.Done():
			watcher.Close()
			return
		case resp := <-watchChan:
			p.handleWatchEvents(resp.Events)
		}
	}
}

// handleWatchEvents 处理服务监听 监听到的事件
func (p *ClientPicker) handleWatchEvents(events []*clientv3.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, event := range events {
		addr := string(event.Kv.Value)
		if addr == p.selfAddr {
			continue
		}
		switch event.Type {
		case clientv3.EventTypePut:
			if _, ok := p.clients[addr]; !ok {
				// 如果不存在 说明是新加的服务实例
				p.put(addr)
				logrus.Infof("new services discovered at %s", addr)
			}
		case clientv3.EventTypeDelete:
			if client, ok := p.clients[addr]; ok {
				// 如果存在 则移除该服务实例
				client.Close()
				p.remove(addr)
				logrus.Infof("service removed at %s", addr)
			}
		}
	}
}

// parseAddrFromKey 根据etcd key中解析地址
func parseAddrFromKey(key string, serviceName string) string {
	prefix := fmt.Sprintf("/services/%s/", serviceName)
	if strings.HasPrefix(key, prefix) {
		// 取出前缀 即为addr
		return strings.TrimPrefix(key, prefix)
	}
	return ""
}
