# GoCache 使用文档

GoCache 的主要用户入口是 `goch.Group`。一个缓存组就是一个独立命名空间，用户通过缓存组执行 `Get`、`Put`、`Delete`、`Clear` 等操作。`goch.Server`、`goch.ClientPicker` 和 `goch.Client` 主要用于分布式节点之间的注册发现和 gRPC 通信，不建议业务代码直接绕过 `Group` 调用 gRPC client。

用法请参考 `example/local_cache/main.go` 文件和 `example/distributed_node/main.go` 文件。

## 本地缓存组

创建缓存组：

```go
group := goch.NewGroup("scores", 1<<20, goch.GetterFunc(
	func(ctx context.Context, key string) ([]byte, error) {
		return loadFromDB(key)
	},
), goch.WithExpr(time.Minute))
```

常用接口：

- `goch.NewGroup(name string, cacheBytes int64, getter goch.Getter, opts ...goch.GroupOption) *goch.Group`：创建缓存组。`getter` 是缓存未命中时的数据源回调。
- `goch.GetGroup(name string) *goch.Group`：按名称获取已创建的缓存组。
- `group.Get(ctx, key) (goch.ByteView, error)`：先查本地缓存，未命中时通过 peer 或 getter 加载。
- `group.Put(ctx, key, value) error`：写入当前缓存组；分布式模式下会同步到该 key 对应的 peer。
- `group.Delete(ctx, key) error`：删除当前缓存组中的 key；分布式模式下会同步删除。
- `group.Clear()`：清空当前组本地缓存。
- `group.Close() error`：关闭当前组并从全局缓存组表移除。
- `group.Stats() map[string]interface{}`：查看命中、加载、peer 等统计信息。

`ByteView` 是只读缓存值视图：

- `view.String()`：按字符串读取。
- `view.GetBytes()`：获取字节切片拷贝。
- `view.Len()`：获取长度。

## 配置选项

缓存组配置：

- `goch.WithExpr(d time.Duration)`：设置缓存过期时间；不设置或为 `0` 表示不过期。
- `goch.WithPeers(peers goch.PeerPicker)`：启用分布式 peer 选择器。
- `goch.WithCacheOptions(opts goch.CacheOptions)`：设置底层缓存实现、容量、清理间隔等。

服务端配置：

- `goch.NewServer(addr, serviceName, opts...)`：创建 gRPC 缓存节点服务，并注册健康检查。
- `goch.WithEtcdEndpoints([]string{...})`：配置创建服务端 etcd client 使用的 endpoints。
- `goch.WithDialTimeout(timeout)`：配置 etcd 连接超时。
- `goch.WithTLS(certFile, keyFile)`：为 gRPC server 启用 TLS。

当前注册发现默认使用 `localhost:2379` 的 etcd。示例中使用默认 etcd 配置。

## 分布式使用方式

每个缓存节点通常按这个顺序启动：

1. 创建 `ClientPicker`，用于从 etcd 发现其他节点并根据 key 选择 peer。
2. 创建同名 `Group`，并通过 `goch.WithPeers(picker)` 注入 peer 选择器。
3. 创建并启动 `Server`，把当前节点注册到 etcd，并通过 gRPC 接收其他节点请求。

示例命令：

```bash
go run ./example/distributed_node -addr 127.0.0.1:18001 -service go-cache-example
go run ./example/distributed_node -addr 127.0.0.1:18002 -service go-cache-example -probe-key key3
```

第二个节点执行 `Group.Get("key3")` 时，会先经过一致性哈希选择负责该 key 的节点；如果选择到远端节点，就通过 gRPC 请求远端节点的同名缓存组。

## 注意事项

- 同一个分布式集群中的节点应使用相同的 `serviceName` 和相同的缓存组名。
- 建议显式使用 `127.0.0.1:port` 或真实可访问 IP 作为 `addr`，不要混用 `localhost`、`127.0.0.1` 和局域网 IP，否则当前节点地址匹配可能不一致。
- `goch.Client` 实现了 `Peer`，主要由 `ClientPicker` 内部使用；业务代码优先使用 `Group`。
- `Server.Get`、`Server.Put`、`Server.Delete` 是 gRPC 暴露给 peer 的接口，不是普通业务入口。
