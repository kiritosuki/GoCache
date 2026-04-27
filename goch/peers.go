package goch

import (
	"context"
	"sync"
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
	selfAddr string
	svcName  string
	mu       sync.Mutex
	// TODO 补充剩余字段
}
