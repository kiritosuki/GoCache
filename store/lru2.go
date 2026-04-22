package store

import (
	"sync"
	"time"
)

const (
	DefaultLRU2BucketCount  = 16
	DefaultLRU2CapPerBucket = 1024
	DefaultLRU2Level2Cap    = 1024
	DefaultLRU2CleanupGap   = 1 * time.Minute
)

type node struct {
	key    string
	value  Value
	exprAt int64 // 过期时间戳 exprAt=0表示已删除
}

type cache struct {
	// link[0]是哨兵节点 记录链表头尾
	link [][2]uint16       // 双向链表 0表示前驱 1表示后继
	m    []node            // 预分配内存存储节点
	hmap map[string]uint16 // 键到节点索引的映射
	last uint16            // 最后一个节点元素的索引
}

type lru2Cache struct {
	locks       []sync.Mutex
	caches      [][2]*cache
	onEvicted   func(key string, value Value)
	cleanupTick *time.Ticker
	mask        int32 // 用于hash取模的掩码
}

func newLRU2Cache(opts Options) *lru2Cache {
	if opts.BucketCount == 0 {
		opts.BucketCount = DefaultLRU2BucketCount
	}
	if opts.CapPerBucket == 0 {
		opts.CapPerBucket = DefaultLRU2CapPerBucket
	}
	if opts.Level2Cap == 0 {
		opts.Level2Cap = DefaultLRU2Level2Cap
	}
	if opts.CleanupGap == 0 {
		opts.CleanupGap = DefaultLRU2CleanupGap
	}
}
