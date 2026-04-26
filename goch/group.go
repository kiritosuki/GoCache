package goch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kiritosuki/GoCache/utils/singleflight"
	"github.com/sirupsen/logrus"
)

var (
	mu     sync.Mutex                // 全局互斥锁
	groups = make(map[string]*Group) // 缓存组集合
	// error 键不能为空
	ErrKeyRequired = errors.New("key is required")
	// error 值不能为空
	ErrValueRequired = errors.New("value is required")
	// error 组已经关闭
	ErrGroupClosed = errors.New("cache group is closed")
)

// Group 每一个缓存组是一个缓存命名空间 隔离不同类型的数据
type Group struct {
	name      string
	getter    Getter
	mainCache *Cache
	peers     PeerPicker
	loader    *singleflight.Loader
	expr      time.Duration // 缓存过期时间，0表示永不过期
	closed    int32         // 原子变量，标记组是否已关闭，1表示关闭，0表示正常运行
	stats     groupStats    // 统计信息
}

// groupStats 保存组的统计信息
type groupStats struct {
	loads        int64 // 加载次数
	localHits    int64 // 本地缓存命中次数
	localMisses  int64 // 本地缓存未命中次数
	peerHits     int64 // 从对等节点获取成功次数
	peerMisses   int64 // 从对等节点获取失败次数
	loaderHits   int64 // 从加载器获取成功次数
	loaderErrors int64 // 从加载器获取失败次数
	loadDuration int64 // 加载总耗时（纳秒）
}

/* 接口型函数 */

// Getter 加载键值的回调函数接口
type Getter interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// GetterFunc 函数类型实现 Getter 接口
type GetterFunc func(ctx context.Context, key string) ([]byte, error)

func (f GetterFunc) Get(ctx context.Context, key string) ([]byte, error) {
	return f(ctx, key)
}

/* Group的配置函数 函数式选项 */

type GroupOption func(group *Group)

// WithExpr 设置缓存过期时间
func WithExpr(d time.Duration) GroupOption {
	return func(group *Group) {
		group.expr = d
	}
}

// WithPeers 设置分布式节点
func WithPeers(peers PeerPicker) GroupOption {
	return func(group *Group) {
		group.peers = peers
	}
}

// WithCacheOptions 设置缓存选项
func WithCacheOptions(opts CacheOptions) GroupOption {
	return func(group *Group) {
		group.mainCache = NewCache(opts)
	}
}

/* 实现 Peer 接口 */

// NewGroup 创建新的 Group 实例
func NewGroup(name string, cacheBytes int64, getter Getter, opts ...GroupOption) *Group {
	if getter == nil {
		panic("Getter 不能为空！")
	}
	// 创建默认缓存选项
	cacheOpts := DefaultCacheOptions()
	cacheOpts.MaxBytes = cacheBytes

	g := &Group{
		name:      name,
		getter:    getter,
		mainCache: NewCache(cacheOpts),
		loader:    &singleflight.Loader{},
	}
	// 应用选项
	for _, opt := range opts {
		opt(g)
	}
	// 注册到全局
	mu.Lock()
	defer mu.Unlock()
	if _, ok := groups[name]; ok {
		logrus.Warnf("Group with name %s already exists, will be replaced", name)
	}
	groups[name] = g
	logrus.Infof("Created cache group [%s] with cacheBytes=%d, expiration=%v", name, cacheBytes, g.expr)
	return g
}

// GetGroup 获取指定名称的缓存组
func GetGroup(name string) *Group {
	mu.Lock()
	defer mu.Unlock()
	return groups[name]
}

// Get 缓存组从缓存中获取数据
func (g *Group) Get(ctx context.Context, key string) (ByteView, error) {
	// 检查缓存组是否关闭
	if atomic.LoadInt32(&g.closed) == 1 {
		return ByteView{}, ErrGroupClosed
	}
	if key == "" {
		return ByteView{}, ErrKeyRequired
	}
	// 先从本地缓存中获取
	view, ok := g.mainCache.Get(ctx, key)
	if ok {
		// 本地缓存命中
		atomic.AddInt64(&g.stats.localHits, 1)
		return view, nil
	}
	// 本地缓存未命中
	atomic.AddInt64(&g.stats.localMisses, 1)
	// 加载数据
	return g.load(ctx, key)
}

// Put 向缓存组中添加键值对
func (g *Group) Put(ctx context.Context, key string, value []byte) error {
	// 检查缓存组是否关闭
	if atomic.LoadInt32(&g.closed) == 1 {
		return ErrGroupClosed
	}
	if key == "" {
		return ErrKeyRequired
	}
	if len(value) == 0 {
		return ErrValueRequired
	}
	// 加入本地缓存
	view := ByteView{bytes: cloneBytes(value)}
	if g.expr > 0 {
		g.mainCache.AddWithExpr(key, view, time.Now().Add(g.expr))
	} else {
		g.mainCache.Add(key, view)
	}
	// 检查是否是从其他节点同步过来的请求
	isPeerRequest := ctx.Value("from_peer") != nil
	// 如果不是从其他节点同步过来的请求 且启用了分布式模式 同步到其他节点
	if !isPeerRequest && g.peers != nil {
		go g.syncToPeers(ctx, "put", key, value)
	}
	return nil
}

// Delete 删除缓存组中的键值对
func (g *Group) Delete(ctx context.Context, key string) error {
	// 检查缓存组是否关闭
	if atomic.LoadInt32(&g.closed) == 1 {
		return ErrGroupClosed
	}
	if key == "" {
		return ErrKeyRequired
	}
	// 从本地缓存中删除
	g.mainCache.Delete(key)
	// 检查是否是从其他节点同步过来的请求
	isPeerRequest := ctx.Value("from_peer") != nil
	// 如果不是从其他节点同步过来的请求 且启用了分布式模式 同步到其他节点
	if !isPeerRequest && g.peers != nil {
		go g.syncToPeers(ctx, "delete", key, nil)
	}
	return nil
}

// Clear 清空缓存
func (g *Group) Clear() {
	// 检查缓存组是否关闭
	if atomic.LoadInt32(&g.closed) == 1 {
		return
	}
	g.mainCache.Clear()
	logrus.Infof("[GoCache] cleared cache for group: [%s]", g.name)
}

// Close 关闭缓存组并释放资源
func (g *Group) Close() error {
	// CAS 判断
	// 如果未关闭则关闭并返回 true
	// 如果已关闭则直接返回 false
	if !atomic.CompareAndSwapInt32(&g.closed, 0, 1) {
		// 缓存组已关闭
		return nil
	}
	// 关闭缓存组后
	// 关闭本地缓存
	if g.mainCache != nil {
		g.mainCache.Close()
	}
	// 从全局map中移除该缓存组
	mu.Lock()
	delete(groups, g.name)
	mu.Unlock()
	logrus.Infof("[GoCache] closed cache group: [%s]", g.name)
	return nil
}

/* 辅助函数 */
// load 加载数据
// 调用 loadData 加载数据 再写回本地缓存
func (g *Group) load(ctx context.Context, key string) (ByteView, error) {
	// 使用 singleflight 确保并发请求只加载一次 防止缓存击穿
	startTime := time.Now()
	viewi, err := g.loader.Do(key, func() (interface{}, error) {
		return g.loadData(ctx, key)
	})
	// 记录加载用时
	loadDuration := time.Since(startTime).Nanoseconds()
	atomic.AddInt64(&g.stats.loadDuration, loadDuration)
	atomic.AddInt64(&g.stats.loads, 1)
	if err != nil {
		atomic.AddInt64(&g.stats.loaderErrors, 1)
		return ByteView{}, err
	}
	view := viewi.(ByteView)
	// 写回本地缓存
	if g.expr > 0 {
		// 缓存有过期时间
		g.mainCache.AddWithExpr(key, view, time.Now().Add(g.expr))
	} else {
		// 缓存没有过期时间
		g.mainCache.Add(key, view)
	}
	return view, nil
}

// loadData 实际加载数据的方法
// 先从其他节点加载 再从数据源加载
func (g *Group) loadData(ctx context.Context, key string) (ByteView, error) {
	// 先尝试从远程节点获取
	if g.peers != nil {
		peer, ok, isSelf := g.peers.PickPeer(key)
		// 如果有远程节点
		if ok && !isSelf {
			value, err := g.getFromPeer(ctx, peer, key)
			if err != nil {
				atomic.AddInt64(&g.stats.peerMisses, 1)
				logrus.Warnf("[GoCache] failed to get from peer: %v", err)
			} else {
				// 如果从远程节点获取数据成功
				atomic.AddInt64(&g.stats.peerHits, 1)
				return value, nil
			}
		}
	}
	// 如果从远程节点获取数据失败 从数据源获取
	bytes, err := g.getter.Get(ctx, key)
	if err != nil {
		return ByteView{}, fmt.Errorf("failed to get from data source: %w", err)
	}
	atomic.AddInt64(&g.stats.loaderHits, 1)
	return ByteView{bytes: cloneBytes(bytes)}, nil
}

// getFromPeer 从其他节点获取数据
func (g *Group) getFromPeer(ctx context.Context, peer Peer, key string) (ByteView, error) {
	bytes, err := peer.Get(g.name, key)
	if err != nil {
		return ByteView{}, fmt.Errorf("failed to get from peer: %w", err)
	}
	return ByteView{bytes: bytes}, nil
}

// syncToPeers 同步操作到其他节点
func (g *Group) syncToPeers(ctx context.Context, op string, key string, value []byte) {
	if g.peers == nil {
		return
	}
	// 选择对等节点
	peer, ok, isSelf := g.peers.PickPeer(key)
	if !ok || isSelf {
		return
	}
	// 创建同步请求上下文
	syncCtx := context.WithValue(context.Background(), "from_peer", true)
	var err error
	switch op {
	case "put":
		err = peer.Put(syncCtx, g.name, key, value)
	case "delete":
		_, err = peer.Delete(g.name, key)
	}
	if err != nil {
		logrus.Errorf("[GoCache] failed to sync: %s to peer: %v", op, err)
	}
}
