package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultLRU2BucketCount  = 16
	DefaultLRU2CapPerBucket = 1024
	DefaultLRU2Level2Cap    = 1024
	DefaultLRU2CleanupGap   = 1 * time.Minute
	Prev                    = uint16(0)
	Next                    = uint16(1)
	ClockResetGap           = 100 * time.Millisecond
)

type node struct {
	key    string
	value  Value
	exprAt int64 // 过期时间戳 exprAt=0表示已删除 exprAt<0表示无限时间
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

/* 全局时钟 */
var clock = time.Now().UnixNano()

// 返回clock的值 原子操作
func Now() int64 {
	return atomic.LoadInt64(&clock)
}

// 初始化协程 用于校准全局时钟
func init() {
	go func() {
		for {
			// 每秒校准一次
			atomic.StoreInt64(&clock, time.Now().UnixNano())
			for i := 0; i < 9; i++ {
				time.Sleep(ClockResetGap)
				atomic.AddInt64(&clock, int64(ClockResetGap))
			}
			time.Sleep(ClockResetGap)
		}
	}()
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
	node2, stat2 := s.get(key, index, 1)
	if stat2 > 0 && node2 != nil {
		if node2.exprAt > 0 && now >= node2.exprAt {
			// 项目已过期 删除
			s.delete(key, index)
			fmt.Println("找到项目已过期，删除")
			return nil, false
		}
		return node2.value, true
	}
	return nil, false
}

func (s *lru2Cache) Put(key string, value Value) error {
	return s.PutWithExpiration(key, value, 0)
}

// expr <= 0 表示设置无限长的过期时间
func (s *lru2Cache) PutWithExpiration(key string, value Value, expr time.Duration) error {
	// 计算过期时间
	exprAt := int64(0)
	if expr <= 0 {
		exprAt = -1
	} else {
		exprAt = Now() + expr.Nanoseconds()
	}
	index := hashBKRD(key) & s.mask
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	// 放入一级缓存
	s.caches[index][0].put(key, value, exprAt, s.onEvicted)
	return nil
}

func (s *lru2Cache) Delete(key string) bool {
	index := hashBKRD(key) & s.mask
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	return s.delete(key, index)
}

func (s *lru2Cache) Clear() {
	var keys []string
	// 遍历桶
	for i := range s.caches {
		s.locks[i].Lock()
		// 遍历一级缓存中的所有key 添加到keys
		s.caches[i][0].walk(func(key string, value Value, exprAt int64) bool {
			keys = append(keys, key)
			return true
		})
		// 遍历二级缓存中的所有key
		s.caches[i][1].walk(func(key string, value Value, exprAt int64) bool {
			// 检查key是否已经被收集过了 避免重复
			for _, k := range keys {
				if key == k {
					return true
				}
			}
			keys = append(keys, key)
			return true
		})
		s.locks[i].Unlock()
	}
	for _, key := range keys {
		s.Delete(key)
	}
}

func (s *lru2Cache) Len() int {
	count := 0
	for i := range s.caches {
		s.locks[i].Lock()
		s.caches[i][0].walk(func(key string, value Value, exprAt int64) bool {
			count++
			return true
		})
		s.caches[i][1].walk(func(key string, value Value, exprAt int64) bool {
			count++
			return true
		})
		s.locks[i].Unlock()
	}
	return count
}

func (s *lru2Cache) Close() {
	if s.cleanupTick != nil {
		s.cleanupTick.Stop()
	}
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
	if index, ok := c.hmap[key]; ok && c.nodes[index].exprAt != 0 {
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

// 从桶中获取对应节点和状态
func (c *cache) get(key string) (*node, int) {
	if index, ok := c.hmap[key]; ok {
		c.movetoFront(index)
		return &(c.nodes[index]), 1
	}
	return nil, 0
}

// 向缓存中添加项 如果是新增则返回1 更新则返回0
func (c *cache) put(key string, value Value, exprAt int64, onEvicted func(key string, value Value)) int {
	// 如果key对应的项目存在 更新值并调整位置
	if index, ok := c.hmap[key]; ok {
		c.nodes[index].value = value
		c.nodes[index].exprAt = exprAt
		c.movetoFront(index)
		return 0
	}
	// 如果key对应的项目不存在 需要新增 判断缓存是否已满
	// 缓存已满
	if c.last == uint16(cap(c.nodes)-1) {
		tailIndex := c.link[0][Prev]
		tail := &(c.nodes[tailIndex])
		// 清理回调
		if onEvicted != nil && tail.exprAt != 0 {
			onEvicted(tail.key, tail.value)
		}
		// 从hmap中移除最末条目
		delete(c.hmap, tail.key)
		c.hmap[key] = tailIndex
		// 在nodes和link中不需要删除再添加 直接修改tail为新添加的项目内容即可
		tail.key = key
		tail.value = value
		tail.exprAt = exprAt
		c.movetoFront(tailIndex)
		return 1
	}
	// 缓存未满 分配新位置 头插法
	c.last++
	curr := c.last
	if len(c.hmap) <= 0 {
		// 如果是第一次插入
		c.link[0][Next] = curr
		c.link[0][Prev] = curr
		c.link[curr] = [2]uint16{0, 0}
	} else {
		// 如果不是第一次插入
		c.link[c.link[0][Next]][Prev] = curr
		c.link[curr] = [2]uint16{0, c.link[0][Next]}
		c.link[0][Next] = curr
	}
	c.nodes[curr].key = key
	c.nodes[curr].value = value
	c.nodes[curr].exprAt = exprAt
	c.hmap[key] = curr
	return 1
}

// 遍历该桶中的所有有效项 无效项会被跳过 不会对无效项执行walker
// 也可以用于查找符合条件的有效项
// 当找到需要的有效项时 可以让调用处walker返回false来提前退出循环
func (c *cache) walk(walker func(key string, value Value, exprAt int64) bool) {
	for index := c.link[0][Next]; index != 0; index = c.link[index][Next] {
		if c.nodes[index].exprAt != 0 && !walker(c.nodes[index].key, c.nodes[index].value, c.nodes[index].exprAt) {
			return
		}
	}
}

// 根据键和桶的序号和缓存等级获取节点和对应状态
func (s *lru2Cache) get(key string, index int32, level int32) (*node, int) {
	if node, stat := s.caches[index][level].get(key); stat > 0 && node != nil {
		now := Now()
		if node.exprAt == 0 || (node.exprAt > 0 && now >= node.exprAt) {
			// 过期或者已删除
			return nil, 0
		}
		return node, stat
	}
	return nil, 0
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
	return deleted
}

// 定时清理过期缓存
func (s *lru2Cache) cleaner() {
	for range s.cleanupTick.C {
		now := Now()
		// 遍历桶
		for i := range s.caches {
			s.locks[i].Lock()
			var expiredKeys []string
			// 遍历一级缓存 查找过期key
			s.caches[i][0].walk(func(key string, value Value, exprAt int64) bool {
				if exprAt > 0 && now >= exprAt {
					expiredKeys = append(expiredKeys, key)
				}
				return true
			})
			// 遍历二级缓存 查找过期key
			s.caches[i][1].walk(func(key string, value Value, exprAt int64) bool {
				if exprAt > 0 && now >= exprAt {
					for _, k := range expiredKeys {
						if key == k {
							// 避免重复
							return true
						}
					}
					expiredKeys = append(expiredKeys, key)
				}
				return true
			})
			for _, key := range expiredKeys {
				s.delete(key, int32(i))
			}
			s.locks[i].Unlock()
		}
	}
}
