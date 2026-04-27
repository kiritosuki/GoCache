package consistenthash

import "hash/crc32"

const (
	DefaultConfigReplicas             = 50
	DefaultConfigMinReplicas          = 10
	DefaultConfigMaxReplicas          = 200
	DefaultConfigLoadBalanceThreshold = 0.25
)

// Config 一致性哈希配置信息
type Config struct {
	Replicas             int                      // 每个真实节点对应的虚拟节点数量
	MinReplicas          int                      // 最小虚拟节点数
	MaxReplicas          int                      // 最大虚拟节点数
	HashFunc             func(data []byte) uint32 // 哈希函数
	LoadBalanceThreshold float64                  // 负载均衡阈值 超过此值触发虚拟节点调整
}

// DefaultConfig 默认配置
var DefaultConfig = &Config{
	Replicas:             DefaultConfigReplicas,
	MinReplicas:          DefaultConfigMinReplicas,
	MaxReplicas:          DefaultConfigMaxReplicas,
	HashFunc:             crc32.ChecksumIEEE,
	LoadBalanceThreshold: DefaultConfigLoadBalanceThreshold,
}
