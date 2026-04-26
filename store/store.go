package store

import "time"

const (
	DefaultOptionsMaxBytes               = 8192
	DefaultOptionsCleanupGap             = 1 * time.Minute
	DefaultOptionsBucketCount            = 16
	DefaultOptionsCapPerBucket           = 512
	DefaultOptionsLever2Cap              = 256
	CacheTypeLRU               CacheType = "LRU"
	CacheTypeLRU2              CacheType = "LRU2"
)

type CacheType string

// Value 缓存内容接口
type Value interface {
	Len() int
}

// Store 缓存接口
type Store interface {
	// Get 根据key获取缓存value / 是否存在
	Get(key string) (Value, bool)

	// Put 存入key-value缓存
	Put(key string, value Value) error

	// PutWithExpiration 存入key-value缓存 设置过期时间
	PutWithExpiration(key string, value Value, expr time.Duration) error

	// Delete 根据key删除缓存value
	Delete(key string) bool

	// Clear 清空所有缓存内容
	Clear()

	// Len 获取缓存值数量长度
	Len() int

	// Close 关闭缓存
	Close()
}

// Options 缓存配置项
type Options struct {
	MaxBytes   int64
	CleanupGap time.Duration
	OnEvicted  func(key string, value Value)

	// lru2
	BucketCount  uint16
	CapPerBucket uint16
	Level2Cap    uint16
}

// NewOptions 创建默认 Options
func NewOptions() Options {
	return Options{
		MaxBytes:     DefaultOptionsMaxBytes,
		CleanupGap:   DefaultLRU2CleanupGap,
		OnEvicted:    nil,
		BucketCount:  DefaultOptionsBucketCount,
		CapPerBucket: DefaultOptionsCapPerBucket,
		Level2Cap:    DefaultLRU2Level2Cap,
	}
}

// NewStore 创建缓存实例
func NewStore(cacheType CacheType, opts Options) Store {
	switch cacheType {
	case CacheTypeLRU2:
		return newLRU2Cache(opts)
	case CacheTypeLRU:
		return newLRUCache(opts)
	default:
		return newLRUCache(opts)
	}
}
