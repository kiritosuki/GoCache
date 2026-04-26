package goch

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kiritosuki/GoCache/store"
	"github.com/sirupsen/logrus"
)

const (
	DefaultCacheOptionCacheType    = store.CacheTypeLRU2
	DefaultCacheOptionMaxBytes     = 8 * 1024 * 1024 // 8MB
	DefaultCacheOptionBucketCount  = 16
	DefaultCacheOptionCapPerBucket = 512
	DefaultCacheOptionLever2Cap    = 256
	DefaultCacheOptionCleanupGap   = 1 * time.Minute
)

// Cache 是对底层缓存存储的封装
type Cache struct {
	mu          sync.Mutex
	store       store.Store  // 底层存储实现
	opts        CacheOptions // 缓存配置选项
	hits        int64        // 缓存命中次数
	misses      int64        // 缓存未命中次数
	initialized int32        // 原子变量，标记缓存是否已初始化，0表示未初始化，1表示初始化
	closed      int32        // 原子变量，标记缓存是否已关闭，1表示关闭，0表示正常运行
}

// CacheOptions 缓存配置选项
type CacheOptions struct {
	CacheType    store.CacheType                     // 缓存类型: LRU, LRU2 等
	MaxBytes     int64                               // 最大内存使用量
	BucketCount  uint16                              // 缓存桶数量 (用于 LRU2)
	CapPerBucket uint16                              // 每个缓存桶的容量 (用于 LRU2)
	Level2Cap    uint16                              // 二级缓存桶的容量 (用于 LRU2)
	CleanupGap   time.Duration                       // 清理间隔
	OnEvicted    func(key string, value store.Value) // 清理回调
}

// DefaultCacheOptions 创建默认的缓存配置
func DefaultCacheOptions() CacheOptions {
	return CacheOptions{
		CacheType:    DefaultCacheOptionCacheType,
		MaxBytes:     DefaultCacheOptionMaxBytes,
		BucketCount:  DefaultCacheOptionBucketCount,
		CapPerBucket: DefaultCacheOptionCapPerBucket,
		Level2Cap:    DefaultCacheOptionLever2Cap,
		CleanupGap:   DefaultCacheOptionCleanupGap,
		OnEvicted:    nil,
	}
}

// NewCache 创建 Cache 实例
func NewCache(opts CacheOptions) *Cache {
	return &Cache{
		opts: opts,
	}
}

// Get 从缓存中获取值
func (c *Cache) Get(ctx context.Context, key string) (ByteView, bool) {
	// 检查本地缓存是否关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		return ByteView{}, false
	}
	// 如果缓存未初始化，直接返回未命中
	if atomic.LoadInt32(&c.initialized) == 0 {
		atomic.AddInt64(&c.misses, 1)
		return ByteView{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 从底层store获取
	value, ok := c.store.Get(key)
	if !ok {
		atomic.AddInt64(&c.misses, 1)
		return ByteView{}, false
	}
	// 转换并返回
	if view, ok := value.(ByteView); ok {
		atomic.AddInt64(&c.hits, 1)
		return view, true
	}
	// 类型断言失败
	logrus.Warnf("类型断言失败，key: %s, 需要 Byteiew 类型", key)
	atomic.AddInt64(&c.misses, 1)
	return ByteView{}, false
}

// AddWithExpr 向缓存中添加带过期时间的键值对
func (c *Cache) AddWithExpr(key string, value ByteView, exprTime time.Time) {
	// 检查本地缓存是否关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		logrus.Warnf("Attemplted to add to a closed cache: %s", key)
		return
	}
	c.ensureInitialized()
	// 计算过期时间
	expr := time.Until(exprTime)
	if expr <= 0 {
		logrus.Debugf("key %s already expired, not adding to cache", key)
		return
	}
	// 加入到底层store
	if err := c.store.PutWithExpiration(key, value, expr); err != nil {
		logrus.Warnf("failed to add key %s to cache with expr: %v", key, err)
	}
}

// Add 向缓存中加入键值对
func (c *Cache) Add(key string, value ByteView) {
	// 检查本地缓存是否关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		logrus.Warnf("Attemplted to add to a closed cache: %s", key)
		return
	}
	c.ensureInitialized()
	if err := c.store.Put(key, value); err != nil {
		logrus.Warnf("failed to add key %s to cache %v", key, err)
	}
}

// Delete 从缓存中删除键值对
func (c *Cache) Delete(key string) bool {
	// 检查本地缓存是否关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		logrus.Warnf("Attemplted to delete to a closed cache: %s", key)
		return false
	}
	// 如果缓存未初始化，直接返回false
	if atomic.LoadInt32(&c.initialized) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store.Delete(key)
}

// Clear 清空缓存
func (c *Cache) Clear() {
	// 检查本地缓存是否关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}
	// 如果缓存未初始化，直接返回
	if atomic.LoadInt32(&c.initialized) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store.Clear()
	// 重置统计信息
	atomic.StoreInt64(&c.hits, 0)
	atomic.StoreInt64(&c.misses, 0)
}

// Close 关闭缓存 释放资源
func (c *Cache) Close() {
	// CAS 判断
	// 如果未关闭则关闭并返回 true
	// 如果已关闭则直接返回 false
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		// 缓存已经关闭了 直接返回
		return
	}
	// 缓存关闭后
	c.mu.Lock()
	defer c.mu.Unlock()
	// 关闭底层store存储
	if c.store != nil {
		// 确保store实现了close接口
		// 如果实现了才关闭 没实现就不管 方便拓展
		if closer, ok := c.store.(interface{ Close() }); ok {
			closer.Close()
		}
		c.store = nil
	}
	// 重置缓存初始化状态
	atomic.StoreInt32(&c.initialized, 0)
	logrus.Debugf("cache closed, hits: %d, misses: %d", atomic.LoadInt64(&c.hits), atomic.LoadInt64(&c.misses))
}

/* 辅助函数 */
// ensureInitialized 检查缓存是否初始化
// 如果已经初始化就直接返回
// 如果未初始化则进行初始化
func (c *Cache) ensureInitialized() {
	// 缓存已初始化
	if atomic.LoadInt32(&c.initialized) == 1 {
		return
	}
	// 初始化缓存
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized == 0 {
		// 创建store选项
		storeOpts := store.Options{
			MaxBytes:     c.opts.MaxBytes,
			CleanupGap:   c.opts.CleanupGap,
			OnEvicted:    c.opts.OnEvicted,
			BucketCount:  c.opts.BucketCount,
			CapPerBucket: c.opts.CapPerBucket,
			Level2Cap:    c.opts.Level2Cap,
		}
		// 创建store实例
		c.store = store.NewStore(c.opts.CacheType, storeOpts)
		// 标记为初始化
		atomic.StoreInt32(&c.initialized, 1)
		logrus.Infof("Cache initailized with type: %s, maxBytes: %d", c.opts.CacheType, c.opts.MaxBytes)
	}
}
