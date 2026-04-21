package store

import (
	"container/list"
	"sync"
	"time"
)

const (
	DefaultRLUCleanupGap = 10 * time.Second  // 单位 s
	DefaultRLUMaxBytes   = 128 * 1024 * 1024 // 128 MB
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
	maxBytes := opts.MaxBytes
	if opts.CleanupGap <= 0 {
		cleanupGap = DefaultRLUCleanupGap
	}
	if opts.MaxBytes <= 0 {
		maxBytes = DefaultRLUMaxBytes
	}
	c := &lruCache{
		list:          list.New(),
		cacheMap:      make(map[string]*list.Element),
		exprMap:       make(map[string]time.Time),
		maxBytes:      maxBytes,
		usedBytes:     0,
		onEvicted:     opts.OnEvicted,
		cleanupGap:    cleanupGap,
		cleanupTicker: time.NewTicker(cleanupGap),
		closeCh:       make(chan struct{}),
	}
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
	// -1 表示无限长的过期时间
	return c.PutWithExpiration(key, value, -1)
}

func (c *lruCache) PutWithExpiration(key string, value Value, expr time.Duration) error {
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
		c.usedBytes += int64(value.Len() - entry.value.Len())
		entry.value = value
		c.list.MoveToFront(elem)
		return nil
	}
	// 如果是新添加的键值对
	entry := &lruEntry{
		key:   key,
		value: value,
	}
	elem := c.list.PushFront(entry)
	c.cacheMap[key] = elem
	c.usedBytes += int64(len([]byte(key)) + value.Len())
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

}

func (c *lruCache) Len() int {
	//TODO implement me
	panic("implement me")
}

func (c *lruCache) Close() {
	//TODO implement me
	panic("implement me")
}

/* 辅助函数 */
// 删除节点 调用时需要持有锁 且保证elem非空
func (c *lruCache) removeElem(elem *list.Element) {
	entry := elem.Value.(*lruEntry)
	delete(c.cacheMap, entry.key)
	delete(c.exprMap, entry.key)
	c.list.Remove(elem)
	c.usedBytes -= int64(len([]byte(entry.key)) + entry.value.Len())
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
