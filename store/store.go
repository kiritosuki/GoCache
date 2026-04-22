package store

import "time"

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

	// PetWithExpiration 存入key-value缓存 设置过期时间
	PutWithExpiration(key string, value Value, expr time.Duration) error

	// Delete 根据key删除缓存value
	Delete(key string) bool

	// Clear 清空所有缓存内容
	Clear()

	// Len 获取缓存值数量长度
	Len() int

	// Close TODO
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
