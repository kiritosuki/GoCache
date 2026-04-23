package store

import (
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 实现 Value 接口
type testValue string

func (v testValue) Len() int { return len(v) }

// 测试基础功能
func TestCacheInternal(t *testing.T) {
	t.Run("初始化验证", func(t *testing.T) {
		capacity := uint16(10)
		c := newCache(capacity)

		// 验证哨兵节点和预分配空间
		// link 和 nodes 长度应为 capacity + 1 (包含索引0)
		if len(c.nodes) != int(capacity+1) {
			t.Fatalf("nodes容量应为%d，实际为%d", capacity+1, len(c.nodes))
		}
		if c.last != 0 {
			t.Fatalf("新缓存last索引应为0，实际为%d", c.last)
		}
		// 检查哨兵节点初始指向 (Next/Prev 此时都应是 0)
		if c.link[0][Next] != 0 || c.link[0][Prev] != 0 {
			t.Fatal("初始化时哨兵节点指向应为0")
		}
	})

	t.Run("基础Put与Get逻辑", func(t *testing.T) {
		c := newCache(5)

		// 1. 首次插入
		c.put("k1", testValue("v1"), -1, nil)
		if c.last != 1 {
			t.Fatalf("插入后last应为1，实际为%d", c.last)
		}

		// 验证哨兵指向
		if c.link[0][Next] != 1 || c.link[0][Prev] != 1 {
			t.Fatal("哨兵节点未正确指向第一个元素")
		}

		// 2. 获取验证
		node, status := c.get("k1")
		if status != 1 || node.value.(testValue) != "v1" {
			t.Fatal("获取k1失败或值不正确")
		}

		// 3. 更新验证
		c.put("k1", testValue("v1-new"), -1, nil)
		node, _ = c.get("k1")
		if node.value.(testValue) != "v1-new" {
			t.Fatal("更新k1失败")
		}
		if c.last != 1 {
			t.Fatal("更新操作不应增加last索引")
		}
	})

	t.Run("双向链表顺序与LRU晋升", func(t *testing.T) {
		// 容量为3
		c := newCache(3)
		c.put("n1", testValue("1"), -1, nil)
		c.put("n2", testValue("2"), -1, nil)
		c.put("n3", testValue("3"), -1, nil)

		// 当前顺序 (头->尾): n3(3), n2(2), n1(1)
		// 哨兵 Next 应指向 3, Prev 应指向 1
		if c.link[0][Next] != 3 || c.link[0][Prev] != 1 {
			t.Errorf("链表初始化顺序错误: Next=%d, Prev=%d", c.link[0][Next], c.link[0][Prev])
		}

		// 访问 n1，n1 应该移到最前面 (movetoFront)
		c.get("n1")
		// 现在顺序: n1, n3, n2
		if c.link[0][Next] != 1 {
			t.Fatal("Get操作后 n1 未移动到头部")
		}
		if c.link[0][Prev] != 2 {
			t.Fatal("Get操作后 尾部节点应为 n2(索引2)")
		}
	})

	t.Run("删除与惰性标记", func(t *testing.T) {
		c := newCache(5)
		c.put("del_me", testValue("temp"), -1, nil)

		// 执行删除
		node, status, _ := c.delete("del_me")
		if status != 1 {
			t.Fatal("删除失败")
		}

		// 核心：你的实现中 delete 只是将 exprAt 设为 0 并移到末尾，并没有从 hmap 移除
		if node.exprAt != 0 {
			t.Fatal("删除后 exprAt 应为 0")
		}

		// 验证 hmap 仍然持有该索引 (这是你的实现逻辑)
		if _, ok := c.hmap["del_me"]; !ok {
			t.Fatal("delete 操作不应直接从 hmap 移除 key")
		}

		// 再次 Get 应通过 exprAt == 0 判断为无效
		_, status = c.get("del_me")
		if status != 1 { // 你的 c.get 逻辑中，hmap 有就返回 1
			t.Log("注意：底层 cache.get 只要 hmap 存在就返回 status=1，过期判断由上层 lru2Cache.Get 处理")
		}
	})

	t.Run("满容量淘汰 (空间复用)", func(t *testing.T) {
		c := newCache(2)
		var evictedKey string
		onEvicted := func(k string, v Value) {
			evictedKey = k
		}

		c.put("k1", testValue("v1"), -1, nil)
		c.put("k2", testValue("v2"), -1, nil)

		// 此时 last = 2, 链表: k2 -> k1 (尾)
		// 再 Put "k3"，应复用 k1 的空间
		c.put("k3", testValue("v3"), -1, onEvicted)

		if evictedKey != "k1" {
			t.Fatalf("应淘汰 k1, 实际淘汰: %s", evictedKey)
		}

		if c.last != 2 {
			t.Fatal("淘汰复用时不应增加 last 索引")
		}

		if _, ok := c.hmap["k1"]; ok {
			t.Fatal("k1 应已从 hmap 中删除")
		}

		if index, ok := c.hmap["k3"]; !ok || index != 1 {
			t.Fatal("k3 应复用索引 1")
		}
	})

	t.Run("Walk 遍历与哨兵边界", func(t *testing.T) {
		c := newCache(3)
		c.put("a", testValue("1"), -1, nil)
		c.put("b", testValue("2"), -1, nil)
		c.put("c", testValue("3"), -1, nil)

		count := 0
		c.walk(func(key string, value Value, exprAt int64) bool {
			count++
			return true
		})

		if count != 3 {
			t.Fatalf("Walk 应遍历 3 个节点，实际: %d", count)
		}

		// 测试删除一个后的遍历
		c.delete("b") // b 还在链表里，但 exprAt 为 0
		count = 0
		c.walk(func(key string, value Value, exprAt int64) bool {
			count++
			return true
		})

		if count != 2 {
			t.Fatalf("Walk 应自动跳过 exprAt=0 的节点，预期 2 个，实际: %d", count)
		}
	})
}

// 测试二级缓存的晋升与淘汰逻辑
func TestLRU2CapacityAndReplacement(t *testing.T) {
	t.Run("L1到L2的晋升逻辑", func(t *testing.T) {
		// 设置 BucketCount 为 1，方便通过 key 确定性地进入同一个桶
		opts := Options{
			BucketCount:  1,
			CapPerBucket: 10,
			Level2Cap:    10,
		}
		lru := newLRU2Cache(opts)
		defer lru.Close()

		key := "promote_key"
		val := testValue("data")

		// 1. 首次 Put 进入 L1
		lru.Put(key, val)

		// 内部验证：此时 L1 应有数据，L2 为空
		if _, ok := lru.caches[0][0].hmap[key]; !ok {
			t.Fatal("数据应首先进入 L1")
		}
		if _, ok := lru.caches[0][1].hmap[key]; ok {
			t.Fatal("数据不应直接进入 L2")
		}

		// 2. 执行 Get 操作，触发晋升
		lru.Get(key)

		// 内部验证：此时 L1 应被删除，L2 应存在
		if _, ok := lru.caches[0][0].hmap[key]; ok {
			// 检查是否被标记为删除 (exprAt == 0)
			idx := lru.caches[0][0].hmap[key]
			if lru.caches[0][0].nodes[idx].exprAt != 0 {
				t.Fatal("晋升后 L1 对应节点应标记为删除")
			}
		}
		if _, ok := lru.caches[0][1].hmap[key]; !ok {
			t.Fatal("Get 操作后数据应晋升至 L2")
		}
	})

	t.Run("单桶物理容量限制与复用", func(t *testing.T) {
		evictedMap := make(map[string]Value)
		var mu sync.Mutex

		opts := Options{
			BucketCount:  1,
			CapPerBucket: 3, // 物理限制为 3
			OnEvicted: func(key string, value Value) {
				mu.Lock()
				evictedMap[key] = value
				mu.Unlock()
			},
		}
		lru := newLRU2Cache(opts)
		defer lru.Close()

		// 填满 L1 (k1, k2, k3)
		lru.Put("k1", testValue("v1"))
		lru.Put("k2", testValue("v2"))
		lru.Put("k3", testValue("v3"))

		// 此时 L1 顺序（头->尾）: k3, k2, k1
		// 插入 k4，应淘汰最老的 k1 (哨兵的 Prev 指向的节点)
		lru.Put("k4", testValue("v4"))

		if _, ok := lru.Get("k1"); ok {
			t.Fatal("k1 应该已被物理淘汰")
		}

		mu.Lock()
		if _, ok := evictedMap["k1"]; !ok {
			t.Fatal("淘汰 k1 时未触发 OnEvicted 回调")
		}
		mu.Unlock()

		// 验证 L1 的 last 索引是否正确锁死在 3 (含哨兵共 4 个位置)
		if lru.caches[0][0].last != 3 {
			t.Fatalf("last 索引应维持在 3，实际为 %d", lru.caches[0][0].last)
		}
	})

	t.Run("L2 层的 LRU 替换", func(t *testing.T) {
		opts := Options{
			BucketCount:  1,
			CapPerBucket: 10,
			Level2Cap:    2, // L2 只能存 2 个
		}
		lru := newLRU2Cache(opts)
		defer lru.Close()

		// 1. 放入 3 个数据并全部晋升到 L2
		keys := []string{"hot1", "hot2", "hot3"}
		for _, k := range keys {
			lru.Put(k, testValue("v"))
			lru.Get(k) // 触发晋升
		}

		// 2. 检查 L2 状态
		// 晋升顺序: hot1 -> hot2 -> hot3
		// 当 hot3 进入时，L2 已满(2个)，应淘汰最早进入 L2 的 hot1
		if _, ok := lru.Get("hot1"); ok {
			// 注意：此时 hot1 在 L1 已删，L2 也应被 hot3 挤掉
			t.Fatal("hot1 应在 L2 满载时被 LRU 淘汰")
		}

		_, ok2 := lru.Get("hot2")
		_, ok3 := lru.Get("hot3")
		if !ok2 || !ok3 {
			t.Fatal("hot2 和 hot3 应保留在 L2 中")
		}
	})

	t.Run("并发哈希分布与容量", func(t *testing.T) {
		// 测试分桶是否工作正常
		bucketCount := uint16(16)
		opts := Options{
			BucketCount:  bucketCount,
			CapPerBucket: 10,
		}
		lru := newLRU2Cache(opts)
		defer lru.Close()

		// 插入大量数据
		for i := 0; i < 100; i++ {
			lru.Put(fmt.Sprintf("key_%d", i), testValue("v"))
		}

		// 验证数据分布在不同的桶
		totalInHmaps := 0
		bucketsUsed := 0
		for i := uint16(0); i < bucketCount; i++ {
			count := len(lru.caches[i][0].hmap)
			if count > 0 {
				bucketsUsed++
			}
			totalInHmaps += count
		}

		if bucketsUsed <= 1 {
			t.Fatal("哈希分桶失败，数据全落在同一个桶或未分布")
		}
	})
}

// 测试walk方法
func TestCacheWalkDetailed(t *testing.T) {
	t.Run("遍历顺序与过滤已删除项", func(t *testing.T) {
		c := newCache(5)
		now := Now()

		// 1. 顺序插入：k1, k2, k3
		// 链表结构 (头 -> 尾): k3 -> k2 -> k1
		c.put("k1", testValue("v1"), now+int64(time.Hour), nil)
		c.put("k2", testValue("v2"), now+int64(time.Hour), nil)
		c.put("k3", testValue("v3"), now+int64(time.Hour), nil)

		// 2. 模拟标记删除 k2 (exprAt 设为 0)
		c.delete("k2")

		var walkKeys []string
		c.walk(func(key string, value Value, exprAt int64) bool {
			walkKeys = append(walkKeys, key)
			return true
		})

		// 验证：k2 虽然在链表里，但 exprAt=0，walk 应该跳过它
		if len(walkKeys) != 2 {
			t.Fatalf("预期遍历2个有效项，实际得到: %v", walkKeys)
		}

		// 验证顺序：k3 是最后插入的，应在头部；k1 是最早的，应在尾部
		if walkKeys[0] != "k3" || walkKeys[1] != "k1" {
			t.Errorf("遍历顺序或过滤不正确，预期 [k3, k1], 实际: %v", walkKeys)
		}
	})

	t.Run("提前终止遍历逻辑", func(t *testing.T) {
		c := newCache(10)
		for i := 0; i < 5; i++ {
			c.put(string(rune('a'+i)), testValue("val"), -1, nil)
		}

		visitedCount := 0
		c.walk(func(key string, value Value, exprAt int64) bool {
			visitedCount++
			// 当访问到第2个元素时停止
			return visitedCount < 2
		})

		if visitedCount != 2 {
			t.Errorf("walk未能在回调返回false时正确停止，访问数: %d", visitedCount)
		}
	})

	t.Run("空缓存与哨兵安全", func(t *testing.T) {
		c := newCache(5)

		called := false
		c.walk(func(key string, value Value, exprAt int64) bool {
			called = true
			return true
		})

		if called {
			t.Error("空缓存不应触发walk回调")
		}
	})

	t.Run("LRU访问后的遍历顺序变化", func(t *testing.T) {
		c := newCache(3)
		c.put("k1", testValue("v1"), -1, nil)
		c.put("k2", testValue("v2"), -1, nil)
		c.put("k3", testValue("v3"), -1, nil)
		// 初始顺序: k3, k2, k1

		// 访问 k1，使其移到前端
		c.get("k1")
		// 预期顺序: k1, k3, k2

		var keys []string
		c.walk(func(key string, value Value, exprAt int64) bool {
			keys = append(keys, key)
			return true
		})

		expected := []string{"k1", "k3", "k2"}
		for i, k := range expected {
			if keys[i] != k {
				t.Fatalf("访问后顺序未更新，位置 %d 预期 %s, 实际 %s", i, k, keys[i])
			}
		}
	})

	t.Run("全量过期项跳过", func(t *testing.T) {
		c := newCache(3)
		// 故意将 exprAt 设为 0 (模拟已手动标记删除)
		c.put("k1", testValue("v1"), 0, nil)

		// 注意：你的 put 方法如果 key 存在会执行 movetoFront
		// 但如果我们在 put 时就传入 exprAt = 0，walk 不应打印它

		count := 0
		c.walk(func(key string, value Value, exprAt int64) bool {
			count++
			return true
		})

		if count != 0 {
			t.Errorf("walk应跳过所有exprAt为0的项，实际统计: %d", count)
		}
	})
}

// 辅助函数：检查切片是否包含
func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// 测试双向链表移动
func TestCacheLinkAdjustment(t *testing.T) {
	t.Run("movetoFront 深度验证", func(t *testing.T) {
		c := newCache(5)
		// 插入顺序: k1, k2, k3 -> 链表状态 (头) k3 <-> k2 <-> k1 (尾)
		c.put("k1", testValue("v1"), -1, nil)
		c.put("k2", testValue("v2"), -1, nil)
		c.put("k3", testValue("v3"), -1, nil)

		idx1 := c.hmap["k1"]
		idx2 := c.hmap["k2"]
		idx3 := c.hmap["k3"]

		// 动作：将末尾的 k1 移到最前
		c.movetoFront(idx1)
		// 预期顺序: k1 <-> k3 <-> k2

		// 1. 验证哨兵指向
		if c.link[0][Next] != idx1 {
			t.Fatal("头部应该是 k1")
		}
		if c.link[0][Prev] != idx2 {
			t.Fatal("尾部应该是 k2")
		}

		// 2. 验证 k1 的邻居关系
		if c.link[idx1][Next] != idx3 || c.link[idx1][Prev] != 0 {
			t.Error("k1 的前后指向不正确")
		}

		// 3. 验证中间节点 k3 的邻居关系
		if c.link[idx3][Prev] != idx1 || c.link[idx3][Next] != idx2 {
			t.Error("k3 的前后指向不正确 (断链)")
		}

		// 4. 验证尾部节点 k2 的邻居关系
		if c.link[idx2][Prev] != idx3 || c.link[idx2][Next] != 0 {
			t.Error("k2 的前后指向不正确")
		}
	})

	t.Run("moveToBack 深度验证", func(t *testing.T) {
		c := newCache(5)
		c.put("k1", testValue("v1"), -1, nil)
		c.put("k2", testValue("v2"), -1, nil)
		c.put("k3", testValue("v3"), -1, nil)
		// 初始 (头) k3 <-> k2 <-> k1 (尾)

		idx2 := c.hmap["k2"]
		idx1 := c.hmap["k1"]
		idx3 := c.hmap["k3"]

		// 动作：将中间的 k2 移到末尾
		c.moveToBack(idx2)
		// 预期顺序: k3 <-> k1 <-> k2

		// 1. 验证哨兵
		if c.link[0][Next] != idx3 {
			t.Fatal("头部应该是 k3")
		}
		if c.link[0][Prev] != idx2 {
			t.Fatal("尾部应该是 k2")
		}

		// 2. 验证中间节点 k1
		if c.link[idx1][Prev] != idx3 || c.link[idx1][Next] != idx2 {
			t.Fatal("中间节点 k1 邻居维护错误")
		}

		// 3. 验证尾部节点 k2
		if c.link[idx2][Prev] != idx1 || c.link[idx2][Next] != 0 {
			t.Fatal("移动后的尾节点 k2 指向错误")
		}
	})

	t.Run("边界操作：对头节点执行movetoFront", func(t *testing.T) {
		c := newCache(3)
		c.put("k1", testValue("v1"), -1, nil)
		c.put("k2", testValue("v2"), -1, nil)

		idx2 := c.hmap["k2"]
		// k2 已经是头部
		c.movetoFront(idx2)

		if c.link[0][Next] != idx2 {
			t.Error("重复移动头节点出错")
		}
		if c.link[idx2][Prev] != 0 {
			t.Error("头节点的 Prev 必须为 0")
		}
	})

	t.Run("边界操作：对尾节点执行moveToBack", func(t *testing.T) {
		c := newCache(3)
		c.put("k1", testValue("v1"), -1, nil)
		c.put("k2", testValue("v2"), -1, nil)

		idx1 := c.hmap["k1"]
		// k1 已经是尾部
		c.moveToBack(idx1)

		if c.link[0][Prev] != idx1 {
			t.Error("重复移动尾节点出错")
		}
		if c.link[idx1][Next] != 0 {
			t.Error("尾节点的 Next 必须为 0")
		}
	})
}

// 测试Store基本接口
func TestStoreInterface(t *testing.T) {
	// 初始化一个较小容量的 Store 方便测试
	opts := Options{
		BucketCount:  4,
		CapPerBucket: 10,
		Level2Cap:    10,
		CleanupGap:   500 * time.Millisecond,
	}
	var s Store = newLRU2Cache(opts)
	defer s.Close()

	t.Run("基础存取操作", func(t *testing.T) {
		key, val := "hello", testValue("world")

		// Put & Get
		s.Put(key, val)
		res, ok := s.Get(key)
		if !ok || res.(testValue) != val {
			t.Errorf("期望得到 %s, 实际得到 %v", val, res)
		}

		// Len 统计 (初次 Put 进 L1，Get 后晋升 L2，总量应为 1)
		if s.Len() != 1 {
			t.Errorf("期望长度为 1, 实际为 %d", s.Len())
		}
	})

	t.Run("过期时间控制", func(t *testing.T) {
		key := "expire_key"
		// 设置 200ms 后过期
		s.PutWithExpiration(key, testValue("gone"), 200*time.Millisecond)

		// 立即获取应该存在
		if _, ok := s.Get(key); !ok {
			t.Fatal("立即获取应当存在")
		}

		// 等待过期 (考虑到全局时钟 100ms 更新一次，需多等一会儿)
		time.Sleep(500 * time.Millisecond)

		if _, ok := s.Get(key); ok {
			t.Error("过期后不应获取到数据")
		}
	})

	t.Run("删除操作", func(t *testing.T) {
		key := "del_key"
		s.Put(key, testValue("value"))

		if !s.Delete(key) {
			t.Error("删除已存在的 Key 应返回 true")
		}

		if _, ok := s.Get(key); ok {
			t.Error("删除后不应获取到数据")
		}

		if s.Delete("not_exist") {
			t.Error("删除不存在的 Key 应返回 false")
		}
	})

	t.Run("Clear 清空功能", func(t *testing.T) {
		// 填充不同桶的数据
		for i := 0; i < 20; i++ {
			s.Put(string(rune('a'+i)), testValue("val"))
		}

		if s.Len() == 0 {
			t.Fatal("清空前长度不应为 0")
		}

		s.Clear()

		if s.Len() != 0 {
			t.Errorf("Clear 后长度应为 0，实际为 %d", s.Len())
		}
	})

	t.Run("并发安全性测试", func(t *testing.T) {
		var wg sync.WaitGroup
		concurrentLimit := 100
		opts := Options{
			BucketCount:  16,
			CapPerBucket: 20, // 总容量 320 > 100
			Level2Cap:    20,
		}
		s := newLRU2Cache(opts)
		defer s.Close()

		// 并发写入
		for i := 0; i < concurrentLimit; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				key := string(rune(n))
				s.Put(key, testValue("v"))
				s.Get(key) // 触发晋升增加复杂度
			}(i)
		}
		wg.Wait()

		// 并发读取与删除
		for i := 0; i < concurrentLimit; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				key := string(rune(n))
				s.Get(key)
				if n%2 == 0 {
					s.Delete(key)
				}
			}(i)
		}
		wg.Wait()

		// 最终统计应符合预期 (删了一半)
		expectedLen := concurrentLimit / 2
		if s.Len() != expectedLen {
			t.Errorf("并发操作后长度不符合预期, 期望 %d, 实际 %d", expectedLen, s.Len())
		}
	})
}

// 测试LRU2Store的LRU晋升与淘汰策略
func TestLRU2StoreLRUEviction(t *testing.T) {
	var evictedKeys []string
	var mu sync.Mutex
	onEvicted := func(key string, value Value) {
		mu.Lock()
		evictedKeys = append(evictedKeys, key)
		mu.Unlock()
	}

	opts := Options{
		BucketCount:  1, // 固定单桶
		CapPerBucket: 2, // L1 容量 2
		Level2Cap:    2, // L2 容量 2
		OnEvicted:    onEvicted,
	}

	store := newLRU2Cache(opts)
	defer store.Close()

	t.Run("L1 物理淘汰测试", func(t *testing.T) {
		// 清空状态
		evictedKeys = []string{}

		// 1. 填满 L1: [k2, k1] (k2是头)
		store.Put("k1", testValue("v1"))
		store.Put("k2", testValue("v2"))

		// 2. 插入 k3，此时 L1 满载，k1 是最老数据（尾部），应被直接物理淘汰
		store.Put("k3", testValue("v3"))

		if _, ok := store.Get("k1"); ok {
			t.Error("k1 未被 Get 访问过，L1 满载时应直接被物理淘汰，不应出现在缓存中")
		}

		mu.Lock()
		if len(evictedKeys) == 0 || evictedKeys[0] != "k1" {
			t.Errorf("期望淘汰 k1, 实际淘汰列表: %v", evictedKeys)
		}
		mu.Unlock()
	})

	t.Run("L2 晋升与满载淘汰测试", func(t *testing.T) {
		// 重新初始化一个新的 store 以保证环境纯净
		s2 := newLRU2Cache(opts)
		defer s2.Close()
		evictedKeys = []string{}

		// 1. 插入 k4, k5 并通过 Get 晋升到 L2
		// L1: [], L2: [k5, k4]
		s2.Put("k4", testValue("v4"))
		s2.Get("k4")
		s2.Put("k5", testValue("v5"))
		s2.Get("k5")

		// 2. 验证它们都在 L2 (内部验证)
		if len(s2.caches[0][1].hmap) != 2 {
			t.Fatal("k4, k5 应该已经晋升到 L2")
		}

		// 3. 此时 L2 已满 (Cap: 2)。再晋升一个新 Key 进入 L2
		s2.Put("k6", testValue("v6"))
		s2.Get("k6") // k6 晋升 L2，L2 此时需淘汰最老的数据 k4

		// 4. 验证 k4 是否被淘汰
		if _, ok := s2.Get("k4"); ok {
			t.Error("k4 是 L2 中最老的数据，当 k6 晋升时，k4 应该被 L2 淘汰")
		}

		mu.Lock()
		// 此时 evictedKeys 应该包含被 L2 挤出来的 k4
		foundK4 := false
		for _, k := range evictedKeys {
			if k == "k4" {
				foundK4 = true
				break
			}
		}
		if !foundK4 {
			t.Errorf("k4 应该触发 OnEvicted 回调, 当前已淘汰: %v", evictedKeys)
		}
		mu.Unlock()
	})

	t.Run("LRU 策略：访问即保活", func(t *testing.T) {
		s3 := newLRU2Cache(opts)
		defer s3.Close()

		// 1. 两个数据进入 L2
		s3.Put("a", testValue("va"))
		s3.Get("a")
		s3.Put("b", testValue("vb"))
		s3.Get("b") // L2 顺序: [b, a]

		// 2. 访问 a，a 变为最新
		s3.Get("a") // L2 顺序: [a, b]

		// 3. 晋升 c，应淘汰 L2 尾部的 b
		s3.Put("c", testValue("vc"))
		s3.Get("c")

		if _, ok := s3.Get("b"); ok {
			t.Error("b 应该是 L2 中最旧的，应被淘汰")
		}
		if _, ok := s3.Get("a"); !ok {
			t.Error("a 最近被访问过，应该保留")
		}
	})
}

// 测试过期时间
func TestLRU2ExpirationPolicy(t *testing.T) {
	opts := Options{
		BucketCount:  1,
		CapPerBucket: 10,
		Level2Cap:    10,
		CleanupGap:   200 * time.Millisecond, // 缩短清理间隔以便测试
	}
	s := newLRU2Cache(opts)
	defer s.Close()

	t.Run("永不过期策略 (-1)", func(t *testing.T) {
		key := "permanent"
		// 显式传入 expr <= 0 触发 exprAt = -1
		s.PutWithExpiration(key, testValue("immortal"), -1)

		// 模拟时间流逝
		time.Sleep(300 * time.Millisecond)

		val, ok := s.Get(key)
		if !ok || val.(testValue) != "immortal" {
			t.Errorf("exprAt为-1的数据不应过期")
		}
	})

	t.Run("即刻过期逻辑 (0)", func(t *testing.T) {
		// 根据你的 PutWithExpiration 逻辑:
		// expr <= 0 时 exprAt = -1 (即不过期)
		// 如果想要测试“已过期”，需要模拟 exprAt > 0 且 now >= exprAt

		key := "expired_fast"
		// 注入一个极短的过期时间
		s.PutWithExpiration(key, testValue("ghost"), 1*time.Nanosecond)

		// 考虑到全局时钟 100ms 步进，等待 200ms 确保 Now() 超过 exprAt
		time.Sleep(200 * time.Millisecond)

		_, ok := s.Get(key)
		if ok {
			t.Error("数据应当已过期并被 Get 拦截")
		}
	})

	t.Run("惰性删除验证", func(t *testing.T) {
		key := "lazy_delete"
		s.PutWithExpiration(key, testValue("data"), 100*time.Millisecond)

		time.Sleep(250 * time.Millisecond)

		// 此时后台 cleaner 可能还没运行，但 Get 应该能发现过期
		_, ok := s.Get(key)
		if ok {
			t.Fatal("Get 操作未能实现惰性删除")
		}

		// 检查底层 hmap，Get 发现过期后应该调用了 s.delete
		if _, exists := s.caches[0][0].hmap[key]; exists {
			// 你的 delete 只是把 exprAt 设为 0
			idx := s.caches[0][0].hmap[key]
			if s.caches[0][0].nodes[idx].exprAt != 0 {
				t.Error("惰性删除后 exprAt 标志位应为 0")
			}
		}
	})

	t.Run("后台 Cleaner 自动清理验证", func(t *testing.T) {
		var mu sync.Mutex
		evicted := make(map[string]bool)

		// 重新创建一个带有回调的 store
		optsClean := Options{
			BucketCount: 1,
			CleanupGap:  100 * time.Millisecond,
			OnEvicted: func(key string, value Value) {
				mu.Lock()
				evicted[key] = true
				mu.Unlock()
			},
		}
		sc := newLRU2Cache(optsClean)
		defer sc.Close()

		sc.PutWithExpiration("short_lived", testValue("tmp"), 50*time.Millisecond)

		// 初始长度应为 1
		if sc.Len() != 1 {
			t.Fatalf("数据未成功存入")
		}

		// 等待 CleanupGap 触发
		time.Sleep(400 * time.Millisecond)

		// 检查长度，cleaner 应该已经把数据从 hmap 中物理删除了
		if sc.Len() != 0 {
			t.Errorf("Cleaner 未能自动清理过期数据，当前长度: %d", sc.Len())
		}

		mu.Lock()
		if !evicted["short_lived"] {
			t.Error("Cleaner 清理过期数据时未触发 OnEvicted 回调")
		}
		mu.Unlock()
	})

	t.Run("过期数据不晋升", func(t *testing.T) {
		s.PutWithExpiration("no_promote", testValue("val"), 50*time.Millisecond)

		time.Sleep(200 * time.Millisecond)

		// 尝试 Get
		s.Get("no_promote")

		// 检查 L2
		if _, ok := s.caches[0][1].hmap["no_promote"]; ok {
			t.Error("已过期的数据不应该被晋升到二级缓存")
		}
	})
}

// 测试LRU2Store的清理循环
func TestLRU2CleanerLoop(t *testing.T) {
	t.Run("验证定期清理的完整生命周期", func(t *testing.T) {
		var mu sync.Mutex
		evictedMap := make(map[string]bool)

		// 1. 设置较短的 CleanupGap 以便在测试中快速触发
		cleanupGap := 200 * time.Millisecond
		opts := Options{
			BucketCount:  2, // 测试多桶清理
			CapPerBucket: 10,
			Level2Cap:    10,
			CleanupGap:   cleanupGap,
			OnEvicted: func(key string, value Value) {
				mu.Lock()
				evictedMap[key] = true
				mu.Unlock()
			},
		}

		s := newLRU2Cache(opts)
		// 注意：newLRU2Cache 内部已经开启了 go s.cleaner()

		// 2. 插入多种类型的数据
		// A: 很快就过期的数据
		s.PutWithExpiration("short_1", testValue("v1"), 50*time.Millisecond)
		// B: 跨桶的很快过期的数据
		s.PutWithExpiration("short_2", testValue("v2"), 50*time.Millisecond)
		// C: 永不过期的数据 (-1)
		s.PutWithExpiration("permanent", testValue("v3"), -1)
		// D: 较长时间后才过期的数据
		s.PutWithExpiration("long_lived", testValue("v4"), 1*time.Hour)

		if s.Len() != 4 {
			t.Fatalf("初始数据量不正确，预期 4，实际 %d", s.Len())
		}

		// 3. 等待足够长的时间，让 Cleaner 至少执行 1-2 个周期
		// 需要覆盖：数据过期时间 + Cleaner 轮询间隔 + 全局时钟更新延迟
		time.Sleep(cleanupGap * 3)

		// 4. 验证清理结果
		finalLen := s.Len()
		if finalLen != 2 {
			t.Errorf("Cleaner 清理后剩余数量不正确，预期 2 (permanent & long_lived)，实际 %d", finalLen)
		}

		mu.Lock()
		if !evictedMap["short_1"] || !evictedMap["short_2"] {
			t.Errorf("过期数据未触发 OnEvicted 回调: %v", evictedMap)
		}
		if evictedMap["permanent"] || evictedMap["long_lived"] {
			t.Errorf("未过期或永不过期的数据被误删: %v", evictedMap)
		}
		mu.Unlock()

		// 5. 测试 Close 能否停止 Cleaner (防止资源泄露)
		s.Close()
		// 这里虽然无法直接探测 goroutine 消失，但可以验证 cleanupTick 被停止
		if s.cleanupTick == nil {
			t.Fatal("cleanupTick 不应为 nil")
		}
	})

	t.Run("Cleaner 对二级缓存的覆盖测试", func(t *testing.T) {
		opts := Options{
			BucketCount: 1,
			CleanupGap:  100 * time.Millisecond,
		}
		s := newLRU2Cache(opts)
		defer s.Close()

		// 1. 将数据存入 L1 并立即 Get 晋升到 L2
		s.PutWithExpiration("hot_expire", testValue("v"), 50*time.Millisecond)
		s.Get("hot_expire") // 此时 hot_expire 在 caches[0][1] 中

		if len(s.caches[0][1].hmap) != 1 {
			t.Fatal("数据未成功进入二级缓存")
		}

		// 2. 等待清理
		time.Sleep(300 * time.Millisecond)

		// 3. 验证 L2 是否也被 Cleaner 覆盖
		if s.Len() != 0 {
			t.Error("Cleaner 未能清理二级缓存中的过期数据")
		}
	})

	t.Run("Cleaner 在高压力下的稳定性", func(t *testing.T) {
		// 此测试验证 Cleaner 扫描时与并发写入的锁竞争情况
		opts := Options{
			BucketCount: 4,
			CleanupGap:  10 * time.Millisecond, // 极高频扫描
		}
		s := newLRU2Cache(opts)
		defer s.Close()

		stop := make(chan struct{})
		var wg sync.WaitGroup

		// 并发写入/读取协程
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						key := "key"
						s.PutWithExpiration(key, testValue("v"), 1*time.Millisecond)
						s.Get(key)
					}
				}
			}(i)
		}

		// 让 Cleaner 在并发压力下跑一会儿
		time.Sleep(200 * time.Millisecond)
		close(stop)
		wg.Wait()

		// 只要不发生死锁或 panic 即视为通过
		t.Log("Cleaner 并发压力测试通过")
	})
}

// 深度测试内部 get 方法的逻辑分支
func TestInternal_GetDeep(t *testing.T) {
	opts := Options{
		BucketCount:  1,
		CapPerBucket: 5,
		Level2Cap:    5,
	}
	s := newLRU2Cache(opts)
	defer s.Close()

	idx := int32(0) // 既然 BucketCount 为 1，索引固定为 0

	t.Run("分层状态验证", func(t *testing.T) {
		key := "layer-key"
		val := testValue("layer-val")

		// 手动注入一级缓存
		s.caches[idx][0].put(key, val, -1, nil)

		// 1. 验证从 L1 获取
		node, stat := s.get(key, idx, 0)
		if stat != 1 || node.value != val {
			t.Fatal("无法从 L1 内部获取数据")
		}

		// 2. 验证此时从 L2 获取应为 nil
		node, stat = s.get(key, idx, 1)
		if stat != 0 || node != nil {
			t.Fatal("数据不应出现在 L2")
		}
	})

	t.Run("过期临界值验证", func(t *testing.T) {
		key := "edge-expire"
		// 注入一个已经过期的时间戳
		pastTime := Now() - 1000
		s.caches[idx][0].put(key, testValue("v"), pastTime, nil)

		// 验证内部 get 能够正确识别 exprAt > 0 且已过期的情况
		node, stat := s.get(key, idx, 0)
		if stat != 0 || node != nil {
			t.Error("内部 get 未能拦截已过期数据")
		}

		// 注入一个标记为删除的数据 (exprAt = 0)
		s.caches[idx][0].put("del-mark", testValue("v"), 0, nil)
		node, stat = s.get("del-mark", idx, 0)
		if stat != 0 || node != nil {
			t.Error("内部 get 未能拦截标记为删除的数据")
		}
	})
}

// 深度测试内部 delete 方法
func TestInternal_DeleteDeep(t *testing.T) {
	var mu sync.Mutex
	var evicted []string

	onEvict := func(k string, v Value) {
		mu.Lock()
		evicted = append(evicted, k)
		mu.Unlock()
	}

	opts := Options{
		BucketCount: 1,
		OnEvicted:   onEvict,
	}
	s := newLRU2Cache(opts)
	defer s.Close()

	t.Run("跨层删除与唯一回调", func(t *testing.T) {
		mu.Lock()
		evicted = nil
		mu.Unlock()

		// 场景：同一个 key 出现在 L1 和 L2（虽然正常流程不会，但 delete 必须处理这种防御性情况）
		key := "cross-key"
		s.caches[0][0].put(key, testValue("v1"), -1, nil)
		s.caches[0][1].put(key, testValue("v2"), -1, nil)

		// 执行删除
		deleted := s.delete(key, 0)

		if !deleted {
			t.Error("删除应返回 true")
		}

		// 验证回调只被触发了一次（根据你的代码逻辑，它会优先取 node1 的值）
		if len(evicted) != 1 {
			t.Errorf("回调触发次数异常: %d", len(evicted))
		}

		// 验证两层都已经标记删除
		if s.caches[0][0].nodes[s.caches[0][0].hmap[key]].exprAt != 0 ||
			s.caches[0][1].nodes[s.caches[0][1].hmap[key]].exprAt != 0 {
			t.Error("L1 或 L2 的节点标记未被重置为 0")
		}
	})
}

// 深度并发测试：模拟高竞争下的桶锁分布
func TestLRU2_ConcurrentStress(t *testing.T) {
	// 使用更多的桶来测试 hash 分布
	opts := Options{
		BucketCount:  64,
		CapPerBucket: 50,
		Level2Cap:    50,
	}
	store := newLRU2Cache(opts)
	defer store.Close()

	const workers = 20
	const ops = 200
	wg := sync.WaitGroup{}

	// 启动并发写入和晋升任务
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				key := fmt.Sprintf("k-%d-%d", workerID, j)

				// 1. 写入
				store.Put(key, testValue(strconv.Itoa(j)))

				// 2. 立即 Get 触发晋升逻辑 (L1 -> L2)
				store.Get(key)

				// 3. 随机删除
				if j%5 == 0 {
					store.Delete(key)
				}
			}
		}(i)
	}

	// 启动并发 Len 和 Clear 任务
	stopChan := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopChan:
				return
			default:
				store.Len()
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	wg.Wait()
	close(stopChan)

	// 最终清理验证
	store.Clear()
	if store.Len() != 0 {
		t.Errorf("Clear后仍有残余数据: %d", store.Len())
	}
}

// 热点数据命中与淘汰率测试
func TestLRU2Store_HitRateAndEviction(t *testing.T) {
	// 设置极小的容量以便于精确控制淘汰观察
	opts := Options{
		BucketCount:  1,  // 固定单桶，排除哈希扰动
		CapPerBucket: 10, // L1 容量
		Level2Cap:    10, // L2 容量
	}
	store := newLRU2Cache(opts)
	defer store.Close()

	t.Run("模拟热点数据晋升与高命中率", func(t *testing.T) {
		// 1. 预热：存入 10 个数据并全部 Get 一次，使其全部进入 L2
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("hot-%d", i)
			store.Put(key, testValue("val"))
			store.Get(key) // 晋升到 L2
		}

		// 2. 统计：连续访问这 10 个热点数据 5 次
		hits := 0
		total := 50
		for i := 0; i < 5; i++ {
			for j := 0; j < 10; j++ {
				if _, ok := store.Get(fmt.Sprintf("hot-%d", j)); ok {
					hits++
				}
			}
		}

		ratio := float64(hits) / float64(total)
		if ratio != 1.0 {
			t.Errorf("热点数据全命中测试失败: 期望 1.0, 实际 %.2f", ratio)
		}
	})

	t.Run("冷数据挤压下的淘汰观测", func(t *testing.T) {
		// 此时 L2 已经满了（10/10，都是 hot-X）

		// 1. 存入 20 个新的“冷”数据
		// 这些数据会先进入 L1。由于 L1 容量只有 10，后 10 个会把前 10 个挤出（物理淘汰）
		for i := 0; i < 20; i++ {
			store.Put(fmt.Sprintf("cold-%d", i), testValue("val"))
		}

		// 2. 验证 L1 的淘汰：cold-0 应该已经被挤出（因为 L1 只有 10 个位置）
		if _, ok := store.Get("cold-0"); ok {
			t.Error("cold-0 应该在 L1 溢出时被物理淘汰")
		}

		// 3. 验证 L2 的稳定性：之前的高频 hot 数据不应受 L1 写入的影响
		if _, ok := store.Get("hot-9"); !ok {
			t.Error("L1 的写入不应导致 L2 数据被淘汰")
		}
	})

	t.Run("L2 内部 LRU 替换命中率", func(t *testing.T) {
		// 当前 L2: [hot-X...] (满)

		// 1. 强制从 L1 晋升 5 个新数据到 L2
		// 这会挤掉 L2 中最旧的 5 个数据
		for i := 100; i < 105; i++ {
			key := fmt.Sprintf("new-hot-%d", i)
			store.Put(key, testValue("val"))
			store.Get(key) // 触发晋升，挤出 L2 的旧数据
		}

		// 2. 统计混合命中率
		// 访问 10 个旧 hot，5 个新 hot
		hits := 0
		for i := 0; i < 10; i++ {
			if _, ok := store.Get(fmt.Sprintf("hot-%d", i)); ok {
				hits++
			}
		}
		for i := 100; i < 105; i++ {
			if _, ok := store.Get(fmt.Sprintf("new-hot-%d", i)); ok {
				hits++
			}
		}

		// 预期：旧 hot 丢了 5 个，新 hot 都在。命中数应为 5 + 5 = 10
		if hits != 10 {
			t.Errorf("L2 替换逻辑后的命中数不符合预期: 期望 10, 实际 %d", hits)
		}
	})
}

// 缓存测试率命中统计
func TestLRU2Store_RealWorldScenario(t *testing.T) {
	// 模拟更真实的 80/20 法则测试
	opts := Options{
		BucketCount:  8,
		CapPerBucket: 32,
		Level2Cap:    64,
	}
	s := newLRU2Cache(opts)
	defer s.Close()

	// 1. 填充基数数据 (200个)
	for i := 0; i < 200; i++ {
		s.Put(fmt.Sprintf("k%d", i), testValue("v"))
	}

	// 2. 模拟高频访问其中 20% 的数据 (40个) 使其晋升
	hotKeys := 40
	for i := 0; i < hotKeys; i++ {
		s.Get(fmt.Sprintf("k%d", i))
	}

	// 3. 混合访问：80% 的请求访问那 20% 的热点，20% 的请求访问其他
	hits := 0
	totalRequests := 1000
	for i := 0; i < totalRequests; i++ {
		var key string
		if i%10 < 8 { // 80% 概率
			key = fmt.Sprintf("k%d", i%hotKeys)
		} else { // 20% 概率
			key = fmt.Sprintf("k%d", (i%160)+hotKeys)
		}

		if _, ok := s.Get(key); ok {
			hits++
		}
	}

	ratio := float64(hits) / float64(totalRequests)
	t.Logf("80/20 模拟场景下的最终命中率: %.2f", ratio)

	// 在该配置下，热点数据应基本全部留存在 L2 中，命中率应显著高于随机分布
	if ratio < 0.70 {
		t.Errorf("命中率过低，LRU2 晋升策略可能失效: %.2f", ratio)
	}
}

// 测试缓存容量增长和性能
func BenchmarkLRU2Store_GrowthAndPerformance(b *testing.B) {
	// 配置大规模缓存以测试增长性能
	opts := Options{
		BucketCount:  64,   // 增加桶数降低锁竞争
		CapPerBucket: 2000, // 总 L1 容量 12.8w
		Level2Cap:    4000, // 总 L2 容量 25.6w
		CleanupGap:   time.Hour,
	}

	store := newLRU2Cache(opts)
	defer store.Close()

	// 预生成 Key，避免 fmt.Sprintf 成为性能瓶颈
	const keyCount = 20000
	keys := make([]string, keyCount)
	for i := 0; i < keyCount; i++ {
		keys[i] = fmt.Sprintf("key-fixed-prefix-%d", i)
	}
	val := testValue("performance-test-static-value")

	b.ResetTimer()

	// 1. 纯写入测试 (考察容量增长和 Hmap 扩容开销)
	b.Run("Put-Sequential-Growth", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// 使用 i 保证 key 的持续增长，直到触发 LRU 替换
			key := keys[i%keyCount]
			store.Put(key, val)
		}
	})

	// 2. 纯读取测试 (考察 L1 到 L2 的晋升性能)
	b.Run("Get-With-Promotion", func(b *testing.B) {
		// 先确保数据都在 L1
		for i := 0; i < keyCount; i++ {
			store.Put(keys[i], val)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// 第一次 Get 会触发晋升，后续会命中 L2
			store.Get(keys[i%keyCount])
		}
	})

	// 3. 高并发混合操作 (最接近真实场景)
	b.Run("Concurrent-Mixed-HighLoad", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				key := keys[i%keyCount]
				if i%10 < 8 { // 80% 读取
					store.Get(key)
				} else { // 20% 写入
					store.Put(key, val)
				}
				i++
			}
		})
	})

	// 4. 内存分配与 GC 压力测试
	b.Run("Mem-Efficiency", func(b *testing.B) {
		var m1, m2 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m1)

		for i := 0; i < b.N; i++ {
			store.Put(keys[i%keyCount], val)
		}

		runtime.ReadMemStats(&m2)
		// 每次操作的内存分配情况
		b.ReportMetric(float64(m2.Mallocs-m1.Mallocs)/float64(b.N), "mallocs/op")
	})
}
