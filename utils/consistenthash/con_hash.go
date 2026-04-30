package consistenthash

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	BalanceGap             = time.Second
	TotalRequestsThreshold = 1000 // 请求数超过此才会触发节点调整
)

type Map struct {
	mu            sync.Mutex
	config        *Config          // 配置信息
	hashList      []int            // 哈希环 存储虚拟节点的哈希值
	hashMap       map[int]string   // 虚拟节点的哈希值到节点的映射
	nodeReplicas  map[string]int   // 节点到虚拟节点数量的映射
	nodeCounts    map[string]int64 // 节点负载统计 每个节点请求了多少次
	totalRequests int64            // 总请求数
}

// Option 函数式选项
type Option func(m *Map)

// WithConfig 设置配置
func WithConfig(config *Config) Option {
	return func(m *Map) {
		m.config = config
	}
}

// New 创建一致性哈希实例
func New(opts ...Option) *Map {
	m := &Map{
		config:       DefaultConfig,
		hashMap:      make(map[int]string),
		nodeReplicas: make(map[string]int),
		nodeCounts:   make(map[string]int64),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.startBalancer() // 启动负载均衡器
	return m
}

/* 上层接口 */

// Add 添加节点
func (m *Map) Add(nodes ...string) error {
	if len(nodes) == 0 {
		return errors.New("no nodes provided")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, node := range nodes {
		if node == "" {
			continue
		}
		// 为节点添加虚拟节点
		m.addNode(node, m.config.Replicas)
	}
	// 重新排序
	sort.Ints(m.hashList)
	return nil
}

// Remove 移除节点
func (m *Map) Remove(node string) error {
	if node == "" {
		return errors.New("invalid node")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remove(node)
}

// Get 根据key获取节点
func (m *Map) Get(key string) string {
	if key == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.hashList) == 0 {
		return ""
	}
	hash := int(m.config.HashFunc([]byte(key)))
	// 二分查找
	// Search会查找第一个满足条件的索引：条件是让第二个函数参数返回true
	// 第一个参数n表示在 [0, n) 范围内查找 如果找不到返回 n
	// 实际上这个二分查找就是找最左插入点的位置
	idx := sort.Search(len(m.hashList), func(i int) bool {
		return m.hashList[i] >= hash
	})
	// 处理边界情况
	if idx == len(m.hashList) {
		// 哈希环
		idx = 0
	}
	node := m.hashMap[m.hashList[idx]]
	count := m.nodeCounts[node]
	m.nodeCounts[node] = count + 1
	atomic.AddInt64(&m.totalRequests, 1)
	return node
}

// GetStats 获取负载统计信息 请求命中各个节点的百分比
func (m *Map) GetStats() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := make(map[string]float64)
	total := atomic.LoadInt64(&m.totalRequests)
	if total == 0 {
		return stats
	}
	for node, count := range m.nodeCounts {
		// 计算请求命中各个节点的百分比
		stats[node] = float64(count) / float64(total)
	}
	return stats
}

/* 辅助函数 */

// addNode 为节点添加虚拟节点
func (m *Map) addNode(node string, replicas int) {
	for i := 0; i < replicas; i++ {
		// 虚拟节点命名为 <node_name>-0  <node_name>-1  <node_name>-2...
		hash := int(m.config.HashFunc([]byte(fmt.Sprintf("%s-%d", node, i))))
		m.hashList = append(m.hashList, hash)
		// 每个虚拟节点的哈希值唯一 可以不同哈希值映射到同一个节点
		m.hashMap[hash] = node
	}
	m.nodeReplicas[node] = replicas
}

// Remove 移除节点
func (m *Map) remove(node string) error {
	if node == "" {
		return errors.New("invalid node")
	}
	replicas := m.nodeReplicas[node]
	if replicas == 0 {
		return fmt.Errorf("node %s not found", node)
	}
	// 移除该节点的所有虚拟节点
	for i := 0; i < replicas; i++ {
		hash := int(m.config.HashFunc([]byte(fmt.Sprintf("%s-%d", node, i))))
		delete(m.hashMap, hash)
		// 删除keys中的哈希值
		for j := 0; j < len(m.hashList); j++ {
			if m.hashList[j] == hash {
				m.hashList = append(m.hashList[:j], m.hashList[(j+1):]...)
				break
			}
		}
	}
	delete(m.nodeReplicas, node)
	delete(m.nodeCounts, node)
	return nil
}

/* 后台协程 负载均衡器 */
func (m *Map) startBalancer() {
	go func() {
		ticker := time.NewTicker(BalanceGap)
		defer ticker.Stop()
		for range ticker.C {
			m.checkAndRebalance()
		}
	}()
}

// checkAndRebalance 检查并重新平衡虚拟节点
func (m *Map) checkAndRebalance() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.totalRequests < TotalRequestsThreshold {
		// 请求数太少 不进行平衡
		return
	}
	// 计算负载情况
	// 平均每个节点平均命中次数
	avgLoad := float64(m.totalRequests) / float64(len(m.nodeReplicas))
	var maxDiff float64
	for _, count := range m.nodeCounts {
		// 计算每个节点实际命中次数与平均命中次数差值
		diff := math.Abs(float64(count) - avgLoad)
		if diff/avgLoad > maxDiff {
			maxDiff = diff / avgLoad
		}
	}
	// 如果负载不平衡程度超过负载均衡阈值 触发虚拟节点调整
	if maxDiff > m.config.LoadBalanceThreshold {
		m.rebalanceNodes(avgLoad)
	}
}

// rebalanceNodes 重新平衡虚拟节点
func (m *Map) rebalanceNodes(avgLoad float64) {
	// 调整每个节点的虚拟节点数量
	for node, count := range m.nodeCounts {
		currReplicas := m.nodeReplicas[node]
		loadRatio := float64(count) / avgLoad
		var newReplicas int
		if loadRatio > 1 {
			// 负载过高 减少虚拟节点
			newReplicas = int(float64(currReplicas) / loadRatio)
		} else {
			// 负载过低 增加虚拟节点
			newReplicas = int(float64(currReplicas) * (2 - loadRatio))
		}
		// 确保在限定范围内
		if newReplicas < m.config.MinReplicas {
			newReplicas = m.config.MinReplicas
		}
		if newReplicas > m.config.MaxReplicas {
			newReplicas = m.config.MaxReplicas
		}
		if newReplicas != currReplicas {
			// 重新添加节点的虚拟节点
			if err := m.remove(node); err != nil {
				// 如果移除失败 就跳过该节点
				continue
			}
			m.addNode(node, newReplicas)
		}
	}
	// 重置计数器
	for node := range m.nodeCounts {
		m.nodeCounts[node] = 0
	}
	m.totalRequests = 0
	// 重新排序
	sort.Ints(m.hashList)
}
