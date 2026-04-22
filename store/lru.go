package store

import (
	"container/list"
	"sync"
	"time"
)

const (
	DefaultLRUCleanupGap = 1 * time.Minute
)

// lruCache LRU缓存 基于hash表 + 双向链表实现
type lruCache struct {
	mu            sync.Mutex
	list          *list.List                    // 双向链表
	cacheMap      map[string]*list.Element      // 缓存映射
	exprMap       map[string]time.Time          // 过期时间映射
	maxBytes      int64                         // 最大容量
	usedBytes     int64                         // 已用容量
	onEvicted     func(key string, value Value) // 淘汰回调
	cleanupGap    time.Duration                 // 清理间隔
	cleanupTicker *time.Ticker                  // 清理计时器
	closeCh       chan struct{}                 // 用于优雅关闭清理协程
}

type lruEntry struct {
	key   string
	value Value
}

// newLRUCache 创建一个新的LRU缓存实例
func newLRUCache(opts Options) *lruCache {
	cleanupGap := opts.CleanupGap
	if opts.CleanupGap <= 0 {
		cleanupGap = DefaultLRUCleanupGap
	}
	c := &lruCache{
		list:       list.New(),
		cacheMap:   make(map[string]*list.Element),
		exprMap:    make(map[string]time.Time),
		maxBytes:   opts.MaxBytes,
		usedBytes:  0,
		onEvicted:  opts.OnEvicted,
		cleanupGap: cleanupGap,
		closeCh:    make(chan struct{}),
	}
	// 启动定期清理过期缓存的协程
	c.cleanupTicker = time.NewTicker(cleanupGap)
	go c.cleaner()
	return c
}

/* 实现store接口 */

func (c *lruCache) Get(key string) (Value, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 获取缓存
	elem, ok := c.cacheMap[key]
	if !ok {
		return nil, false
	}
	// 检查过期情况(如果设置了过期时间)
	if t, ok := c.exprMap[key]; ok && time.Now().After(t) {
		// 已经过期了 就删除缓存
		c.removeElem(elem)
		return nil, false
	}
	// 值存在且未过期 返回
	entry := elem.Value.(*lruEntry)
	value := entry.value
	// 移到头部
	c.list.MoveToFront(elem)
	return value, true
}

func (c *lruCache) Put(key string, value Value) error {
	// 0 表示无限长的过期时间
	return c.PutWithExpiration(key, value, 0)
}

func (c *lruCache) PutWithExpiration(key string, value Value, expr time.Duration) error {
	if value == nil {
		c.Delete(key)
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 过期时间设置为大于0才被视为有效
	if expr > 0 {
		c.exprMap[key] = time.Now().Add(expr)
	} else {
		delete(c.exprMap, key)
	}
	// 如果缓存映射中存在 更新
	if elem, ok := c.cacheMap[key]; ok {
		entry := elem.Value.(*lruEntry)
		oldLen := entry.value.Len()
		c.usedBytes += int64(value.Len() - oldLen)
		entry.value = value
		c.list.MoveToFront(elem)
		if value.Len() > oldLen {
			c.evict()
		}
		return nil
	}
	// 如果是新添加的键值对
	entry := &lruEntry{
		key:   key,
		value: value,
	}
	elem := c.list.PushFront(entry)
	c.cacheMap[key] = elem
	c.usedBytes += int64(len(key) + value.Len())
	// 添加完新项后 检查usedBytes是否变大导致需要淘汰旧项
	c.evict()
	return nil
}

func (c *lruCache) Delete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.cacheMap[key]; ok {
		c.removeElem(elem)
		return true
	}
	return false
}

func (c *lruCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 如果有回调函数 先调用所有回调函数
	if c.onEvicted != nil {
		for _, elem := range c.cacheMap {
			entry := elem.Value.(*lruEntry)
			c.onEvicted(entry.key, entry.value)
		}
	}
	// 删除所有缓存
	c.list.Init()
	c.cacheMap = make(map[string]*list.Element)
	c.exprMap = make(map[string]time.Time)
	c.usedBytes = 0
}

func (c *lruCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.list.Len()
}

func (c *lruCache) Close() {
	if c.cleanupTicker != nil {
		c.cleanupTicker.Stop()
		close(c.closeCh)
	}
}

/* 辅助函数 */
// 删除节点 调用时需要持有锁 且保证elem非空
func (c *lruCache) removeElem(elem *list.Element) {
	entry := elem.Value.(*lruEntry)
	delete(c.cacheMap, entry.key)
	delete(c.exprMap, entry.key)
	c.list.Remove(elem)
	c.usedBytes -= int64(len(entry.key) + entry.value.Len())
	if c.onEvicted != nil {
		c.onEvicted(entry.key, entry.value)
	}
}

// 淘汰超过缓存大小限制的旧值 调用时需要持有锁
func (c *lruCache) evict() {
	// 清理全部过期项
	now := time.Now()
	for k, t := range c.exprMap {
		if now.After(t) {
			// 过期了 删除
			if elem, ok := c.cacheMap[k]; ok {
				c.removeElem(elem)
			}
		}
	}
	// 根据内存限制 清理旧项
	for c.maxBytes > 0 && c.usedBytes > c.maxBytes && c.list.Len() > 0 {
		oldElem := c.list.Back()
		if oldElem != nil {
			c.removeElem(oldElem)
		}
	}
}

/* 常驻任务 */
// 定期清理过期的缓存 该协程常驻 启用时需确保cleanupTicker非空
func (c *lruCache) cleaner() {
	for {
		select {
		case <-c.cleanupTicker.C:
			c.mu.Lock()
			c.evict()
			c.mu.Unlock()
		case <-c.closeCh:
			return
		}
	}
}

/* 非接口方法 */
// GetWithExpiration 获取缓存项及剩余过期时间
func (c *lruCache) GetWithExpiration(key string) (Value, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 获取缓存
	elem, ok := c.cacheMap[key]
	if !ok {
		return nil, 0, false
	}
	entry := elem.Value.(*lruEntry)
	// 检查过期情况(如果设置了过期时间)
	now := time.Now()
	if t, ok := c.exprMap[key]; ok {
		// 有过期时间
		if now.After(t) {
			// 过期了 删除缓存
			c.removeElem(elem)
			return nil, 0, false
		} else {
			// 没过期 计算剩余过期时间
			sub := t.Sub(now)
			c.list.MoveToFront(elem)
			return entry.value, sub, true
		}
	}
	// 没有过期时间
	c.list.MoveToFront(elem)
	return entry.value, 0, true
}

// GetExpiration 获取过期时间
func (c *lruCache) GetExpiration(key string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.exprMap[key]
	return t, ok
}

// UpdateExpiration 更新过期时间为expiration 如果expiration <= 0表示设置无限长的过期时间
func (c *lruCache) UpdateExpiration(key string, expiration time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.exprMap[key]
	if !ok {
		return false
	}
	if expiration <= 0 {
		delete(c.exprMap, key)
	} else {
		c.exprMap[key] = time.Now().Add(expiration)
	}
	return true
}

// GetUsedBytes 返回当前缓存大小
func (c *lruCache) GetUsedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedBytes
}

// GetMaxBytes 返回最大缓存大小
func (c *lruCache) GetMaxBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxBytes
}

// SetMaxBytes 设置最大缓存大小
func (c *lruCache) SetMaxBytes(maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = maxBytes
	// 可能最大缓存变小了 触发淘汰
	if maxBytes > 0 && c.usedBytes > maxBytes {
		c.evict()
	}
}
