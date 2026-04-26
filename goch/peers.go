package goch

import "context"

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
