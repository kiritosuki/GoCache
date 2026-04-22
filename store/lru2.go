package store

import (
	"fmt"
	"sync"
	"time"
)

const (
	DefaultLRU2BucketCount  = 16
	DefaultLRU2CapPerBucket = 1024
	DefaultLRU2Level2Cap    = 1024
	DefaultLRU2CleanupGap   = 1 * time.Minute
	Prev                    = uint16(0)
	Next                    = uint16(1)
)

type node struct {
	key    string
	value  Value
	exprAt int64 // 过期时间戳 exprAt=0表示已删除
}

type cache struct {
	// link[0]是哨兵节点 记录链表头尾
	link  [][2]uint16       // 双向链表 0表示前驱 1表示后继
	nodes []node            // 预分配内存存储节点
	hmap  map[string]uint16 // 键到节点索引的映射
	last  uint16            // 最后一个节点元素的索引
}

type lru2Cache struct {
	locks       []sync.Mutex
	caches      [][2]*cache                   // 二级缓存矩阵 分别放新数据和热点数据 也就是桶
	onEvicted   func(key string, value Value) // 清理回调
	cleanupTick *time.Ticker                  // 清理定时器
	mask        int32                         // 用于hash取模的掩码
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
	if opts.CleanupGap <= 0 {
		opts.CleanupGap = DefaultLRU2CleanupGap
	}
	mask := getMask(opts.BucketCount)
	s := &lru2Cache{
		locks:       make([]sync.Mutex, mask+1),
		caches:      make([][2]*cache, mask+1),
		onEvicted:   opts.OnEvicted,
		cleanupTick: time.NewTicker(opts.CleanupGap),
		mask:        int32(mask),
	}
	for i := range s.caches {
		s.caches[i][0] = newCache(opts.CapPerBucket)
		s.caches[i][1] = newCache(opts.Level2Cap)
	}
	if opts.CleanupGap > 0 {
		// TODO 实现 cleaner
		go s.cleaner()
	}
	return s
}

/* 实现 Store 接口 */

func (s *lru2Cache) Get(key string) (Value, bool) {
	index := hashBKRD(key) & s.mask
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	now := Now()
	// 先检查一级缓存
	node1, stat1, exprAt := s.caches[index][0].delete(key)
	if stat1 > 0 {
		// 在一级缓存中找到该项目
		if exprAt > 0 && now >= exprAt {
			// 项目已过期 删除
			s.delete(key, index)
			fmt.Println("找到项目已过期，删除")
			return nil, false
		}
		// 项目有效 移动到二级缓存
		s.caches[index][1].put(key, node1.value, exprAt, s.onEvicted)
		fmt.Println("项目有效，移动到二级缓存")
		return node1.value, true
	}
	// 一级缓存未查到 查看二级缓存
	s.check(key, index, 1)
}

func (s *lru2Cache) Put(key string, value Value) error {
	//TODO implement me
	panic("implement me")
}

func (s *lru2Cache) PutWithExpiration(key string, value Value, expr time.Duration) error {
	//TODO implement me
	panic("implement me")
}

func (s *lru2Cache) Delete(key string) bool {
	//TODO implement me
	panic("implement me")
}

func (s *lru2Cache) Clear() {
	//TODO implement me
	panic("implement me")
}

func (s *lru2Cache) Len() int {
	//TODO implement me
	panic("implement me")
}

func (s *lru2Cache) Close() {
	//TODO implement me
	panic("implement me")
}

/* 辅助函数 */
// 获取掩码 逻辑是bucket的大小必须为2的幂次方 如果不够就扩容到最近的2的幂次方
// 如果 num 为2的幂次方 则有公式：hash & (num - 1) == hash % num
func getMask(cnt uint16) uint16 {
	// 先判断cnt是不是2的幂次方
	if cnt > 0 && cnt&(cnt-1) == 0 {
		// cnt是2的幂次方
		return cnt - 1
	}
	// cnt不是2的幂次方
	// 把最高位的1复制给右边一位
	cnt = cnt | (cnt >> 1)
	// 把最高位的两个1复制给右边2位...以此类推 最多16位
	cnt = cnt | (cnt >> 2)
	cnt = cnt | (cnt >> 4)
	cnt = cnt | (cnt >> 8)
	return cnt
}

// 获取cache 索引0都是哨兵
func newCache(cap uint16) *cache {
	return &cache{
		link:  make([][2]uint16, cap+1),
		nodes: make([]node, cap+1),
		hmap:  make(map[string]uint16, cap+1),
		last:  0,
	}
}

// BKRD hash算法 返回键的hash值
func hashBKRD(s string) int32 {
	var hash int32
	for i := 0; i < len(s); i++ {
		hash = hash*131 + int32(s[i])
	}
	return hash
}

// 从缓存中删除键对应的项
func (c *cache) delete(key string) (*node, int, int64) {
	// exprAt 是过期时间戳 有效值必须大于0 等于0表示删除
	if index, ok := c.hmap[key]; ok && c.nodes[index].exprAt > 0 {
		e := c.nodes[index].exprAt
		c.nodes[index].exprAt = 0 // 标记为删除
		c.moveToBack(index)
		return &(c.nodes[index]), 1, e
	}
	// 项不存在或已被标记为删除
	return nil, 0, 0
}

// 把节点移动到链表尾部
func (c *cache) moveToBack(index uint16) {
	if c.link[index][Next] != 0 {
		// 让该节点的前一个节点 next 指向该节点的下一个节点
		c.link[c.link[index][Prev]][Next] = c.link[index][Next]
		// 让该节点的下一个节点 prev 指向该节点的前一个节点
		c.link[c.link[index][Next]][Prev] = c.link[index][Prev]
		// 让该节点的 next 指向哨兵
		c.link[index][Next] = 0
		// 让该节点的 prev 指向旧的末尾节点
		c.link[index][Prev] = c.link[0][Prev]
		// 让旧的末尾节点 next 指向该节点
		c.link[c.link[0][Prev]][Next] = index
		// 让哨兵的 prev 指向该节点
		c.link[0][Prev] = index
	}
}

// 把节点移动到链表头部
func (c *cache) movetoFront(index uint16) {
	if c.link[index][Prev] != 0 {
		// 让该节点的前一个节点 next 指向该节点的下一个节点
		c.link[c.link[index][Prev]][Next] = c.link[index][Next]
		// 让该节点的下一个节点 prev 指向该节点的前一个节点
		c.link[c.link[index][Next]][Prev] = c.link[index][Prev]
		// 让该节点的 prev 指向哨兵
		c.link[index][Prev] = 0
		// 让该节点的 next 指向旧的头节点
		c.link[index][Next] = c.link[0][Next]
		// 让旧的头节点 prev 指向该节点
		c.link[c.link[0][Next]][Prev] = index
		// 让哨兵的 next 指向该节点
		c.link[0][Next] = index
	}
}

func (s *lru2Cache) delete(key string, index int32) bool {
	node1, stat1, _ := s.caches[index][0].delete(key)
	node2, stat2, _ := s.caches[index][1].delete(key)
	deleted := stat1 > 0 || stat2 > 0
	if deleted && s.onEvicted != nil {
		if node1 != nil && node1.value != nil {
			s.onEvicted(key, node1.value)
		} else if node2 != nil && node2.value != nil {
			s.onEvicted(key, node2.value)
		}
	}
	// TODO 作者在这里注释掉了一块代码 何意味
	return deleted
}

/* 全局时钟 */
func Now() int64 {
	// TODO 实现全局时钟
}
