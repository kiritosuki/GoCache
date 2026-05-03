# GoCache 分布式链路阅读笔记

下面用一个固定例子讲完整链路。假设运行三个节点：

```text
节点 A addr = "127.0.0.1:18001"
节点 B addr = "127.0.0.1:18002"
节点 C addr = "127.0.0.1:18003"

serviceName = "go-cache"
groupName   = "scores"
key         = "Tom"
value       = []byte("630")
```

## 关键概念

- `addr`：一个 GoCache 节点的 gRPC 地址，也是 etcd 里的服务实例值，也是哈希环里的真实节点名。
- `serviceName`：etcd 服务名，用来把多个节点归为同一个集群。
- `groupName`：缓存组名，用来区分缓存命名空间，比如 `scores`、`users`、`products`。
- 一致性哈希中的节点 `name`：就是 `addr` 字符串，比如 `"127.0.0.1:18002"`。

## 一、三个节点启动时发生什么

每个节点都会执行类似 `example/distributed_node/main.go:30` 里的流程：

```go
picker, err := goch.NewClientPicker(addr, goch.WithServiceName(service))
group := goch.NewGroup(groupName, ..., goch.WithPeers(picker))
server, err := goch.NewServer(addr, service)
go server.Start()
```

以节点 A 为例：

```go
NewClientPicker("127.0.0.1:18001", WithServiceName("go-cache"))
```

进入 `goch/peers.go:64`：

```go
picker := &ClientPicker{
    selfAddr:    addr,
    serviceName: DefaultServiceName,
    consHash:    consistenthash.New(),
    clients:     make(map[string]*Client),
}
```

执行完 option 后，A 的 `ClientPicker` 状态是：

```text
selfAddr    = "127.0.0.1:18001"
serviceName = "go-cache"
consHash    = 空哈希环
clients     = 空 map
```

然后执行：

```go
picker.consHash.Add(addr)
```

所以 A 会先把自己加入一致性哈希环：

```text
A 的哈希环里先有：
"127.0.0.1:18001"
```

B、C 同理，各自启动时也会先把自己加入自己的哈希环。

接着 `NewClientPicker` 创建 etcd client：

```go
clientv3.New(clientv3.Config{
    Endpoints:   register.DefaultConfig.EndPoints,
    DialTimeout: register.DefaultConfig.DialTimeout,
})
```

默认连接：

```text
etcd endpoints = ["localhost:2379"]
```

然后调用：

```go
picker.startServiceDiscovery()
```

对应 `goch/peers.go:106`。它做两件事：

1. `fetchAllServices()`：先全量拉一次已有节点。
2. `go watchServiceChanges()`：后台监听后续节点变化。

## 二、Server 创建和服务注册

每个节点也会创建 `Server`。以 A 为例：

```go
goch.NewServer("127.0.0.1:18001", "go-cache")
```

进入 `goch/server.go:83`。这里会创建：

```go
grpcServer := grpc.NewServer(...)
server := &Server{
    addr:        "127.0.0.1:18001",
    serviceName: "go-cache",
    groups:      make(map[string]*Group),
    grpcServer:  grpcServer,
    etcdCli:     etcdCli,
}
```

然后注册 gRPC 服务：

```go
pb.RegisterGoCacheServer(grpcServer, server)
```

这句话的意思是：以后别的节点通过 gRPC 调用 `GoCache.Get`、`GoCache.Put`、`GoCache.Delete` 时，会落到这个 `server` 对象的方法：

```text
Server.Get
Server.Put
Server.Delete
```

也就是：

- `goch/server.go:157`
- `goch/server.go:170`
- `goch/server.go:192`

然后 `server.Start()` 进入 `goch/server.go:134`：

```go
listener, err := net.Listen("tcp", s.addr)
```

A 开始监听：

```text
127.0.0.1:18001
```

然后注册到 etcd：

```go
register.Register(s.serviceName, s.addr, stopCh)
```

也就是：

```go
register.Register("go-cache", "127.0.0.1:18001", stopCh)
```

进入 `register/register.go:31`。它会写入 etcd：

```go
key := fmt.Sprintf("/services/%s/%s", svcName, addr)
value := addr
```

所以 A 写入：

```text
key   = "/services/go-cache/127.0.0.1:18001"
value = "127.0.0.1:18001"
```

B 写入：

```text
key   = "/services/go-cache/127.0.0.1:18002"
value = "127.0.0.1:18002"
```

C 写入：

```text
key   = "/services/go-cache/127.0.0.1:18003"
value = "127.0.0.1:18003"
```

这些 key 都绑定 etcd lease，靠 `KeepAlive` 续约。如果节点挂了，lease 过期，key 会被删掉。

## 三、服务发现如何让 A/B/C 互相知道

每个节点的 `ClientPicker` 都在监听：

```go
watcher.Watch(p.ctx, "/services/"+p.serviceName, clientv3.WithPrefix())
```

对于 `serviceName = "go-cache"`，监听前缀是：

```text
/services/go-cache
```

假设 A 发现 B，`handleWatchEvents` 收到 etcd PUT 事件：

```text
event.Kv.Value = "127.0.0.1:18002"
```

进入 `goch/peers.go:214`：

```go
addr := string(event.Kv.Value)
```

得到：

```text
addr = "127.0.0.1:18002"
```

如果不是自己：

```go
if addr == p.selfAddr {
    continue
}
```

A 的 `selfAddr` 是 `"127.0.0.1:18001"`，所以 B 是远端节点，于是：

```go
p.put(addr)
```

进入 `goch/peers.go:177`：

```go
client, err := NewClient(addr, p.serviceName, p.etcdCli)
p.consHash.Add(addr)
p.clients[addr] = client
```

对 A 来说，这一步会创建到 B 的 gRPC client：

```go
NewClient("127.0.0.1:18002", "go-cache", p.etcdCli)
```

进入 `goch/client.go:35`。里面核心是：

```go
conn, err := grpc.NewClient(
    addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
    grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
)
grpcClient := pb.NewGoCacheClient(conn)
```

所以 A 的状态变成：

```text
A.consHash 里有：
"127.0.0.1:18001"
"127.0.0.1:18002"
"127.0.0.1:18003"

A.clients 里有：
"127.0.0.1:18002" -> Client{addr: "127.0.0.1:18002", grpcCli: ...}
"127.0.0.1:18003" -> Client{addr: "127.0.0.1:18003", grpcCli: ...}
```

B、C 也会形成类似状态。每个节点的哈希环里都有 A/B/C 三个 `addr`；但 `clients` 里只保存远端节点，不保存自己。

## 四、A 执行 Put，算出 key 属于 B

现在在 A 上执行：

```go
group.Put(ctx, "Tom", []byte("630"))
```

进入 `goch/group.go:151`。这个 `group` 的字段大概是：

```text
g.name  = "scores"
g.peers = A 的 ClientPicker
```

`Put` 会先写 A 本地缓存，然后判断是不是 peer 请求：

```go
isPeerRequest := ctx.Value("from_peer") != nil
```

用户直接调用时没有这个标记，所以：

```text
isPeerRequest = false
```

于是异步同步：

```go
go g.syncToPeers(ctx, "put", key, value)
```

进入 `goch/group.go:383`：

```go
peer, ok, isSelf := g.peers.PickPeer(key)
```

也就是调用 A 的 picker：

```go
A.ClientPicker.PickPeer("Tom")
```

进入 `goch/peers.go:117`：

```go
addr := p.consHash.Get(key)
```

假设一致性哈希算出：

```text
key "Tom" -> "127.0.0.1:18002"
```

也就是 B。然后：

```go
if addr == p.selfAddr {
    return nil, true, true
}
```

A 的 `selfAddr` 是 `"127.0.0.1:18001"`，不等于 B，所以继续：

```go
client, ok := p.clients[addr]
return client, true, false
```

返回：

```text
peer   = A.clients["127.0.0.1:18002"]
ok     = true
isSelf = false
```

回到 `syncToPeers`：

```go
syncCtx := context.WithValue(context.Background(), "from_peer", true)
err = peer.Put(syncCtx, g.name, key, value)
```

实际调用：

```go
Client.Put(syncCtx, "scores", "Tom", []byte("630"))
```

进入 `goch/client.go:94`：

```go
c.grpcCli.Put(ctx, &pb.Request{
    Group: "scores",
    Key:   "Tom",
    Value: []byte("630"),
})
```

这就是 A 到 B 的 gRPC 请求：

```text
method        = GoCache.Put
target        = "127.0.0.1:18002"
request.group = "scores"
request.key   = "Tom"
request.value = "630"
```

## 五、B 收到 A 的 Put

B 的 gRPC server 在 `server.Start()` 里已经：

```go
grpcServer.Serve(listener)
```

并且之前注册过：

```go
pb.RegisterGoCacheServer(grpcServer, server)
```

所以 A 发来的 `GoCache.Put` 会进入 B 的：

```go
Server.Put(ctx, req)
```

对应 `goch/server.go:170`。此时：

```text
req.Group = "scores"
req.Key   = "Tom"
req.Value = []byte("630")
```

B 先找缓存组：

```go
group := GetGroup(req.Group)
```

也就是：

```go
GetGroup("scores")
```

注意：这里用的是全局 group registry，不是 `Server.groups` 字段。所以 B 进程里必须已经创建过同名缓存组：

```go
goch.NewGroup("scores", ..., goch.WithPeers(B_picker))
```

否则这里会报：

```text
group scores not found
```

找到 group 后，B 设置：

```go
ctx = context.WithValue(ctx, "from_peer", true)
```

然后调用：

```go
group.Put(ctx, "Tom", []byte("630"))
```

进入 B 的 `Group.Put`。因为 ctx 里有：

```text
from_peer = true
```

所以 B 写入本地缓存后，不会再次 `syncToPeers`，避免 `A -> B -> C -> A` 这种传播循环。

到这里，B 本地缓存中有：

```text
group "scores"
key   "Tom"
value "630"
```

## 六、C 执行 Get，算出 key 应该去 B

现在 C 上执行：

```go
group.Get(ctx, "Tom")
```

进入 `goch/group.go:129`。流程是：

1. 先查 C 本地缓存。
2. 没命中。
3. 调用 `g.load(ctx, key)`。
4. 进入 `g.loadData(ctx, key)`。

关键在 `goch/group.go:348`：

```go
peer, ok, isSelf := g.peers.PickPeer(key)
```

也就是调用 C 的 picker：

```go
C.ClientPicker.PickPeer("Tom")
```

C 的哈希环里也有：

```text
"127.0.0.1:18001"
"127.0.0.1:18002"
"127.0.0.1:18003"
```

一致性哈希对同一个 key、同一批节点，结果应该一样：

```text
"Tom" -> "127.0.0.1:18002"
```

C 的 `selfAddr` 是：

```text
"127.0.0.1:18003"
```

所以不是自己。于是 `PickPeer` 返回：

```text
peer   = C.clients["127.0.0.1:18002"]
ok     = true
isSelf = false
```

于是 `loadData` 调用：

```go
value, err := g.getFromPeer(ctx, peer, key)
```

进入 `goch/group.go:374`：

```go
bytes, err := peer.Get(g.name, key)
```

实际是：

```go
Client.Get("scores", "Tom")
```

进入 `goch/client.go:70`：

```go
ctx, cancel := context.WithTimeout(context.Background(), GrpcContextTimeout)
resp, err := c.grpcCli.Get(ctx, &pb.Request{
    Group: "scores",
    Key:   "Tom",
})
```

这就是 C 到 B 的 gRPC Get 请求：

```text
method        = GoCache.Get
target        = "127.0.0.1:18002"
request.group = "scores"
request.key   = "Tom"
request.value = 空
```

## 七、B 收到 C 的 Get

B 收到 `GoCache.Get` 后，进入 `goch/server.go:157`：

```go
func (s *Server) Get(ctx context.Context, req *pb.Request) (*pb.ResponseForGet, error) {
    group := GetGroup(req.Group)
    view, err := group.Get(ctx, req.Key)
    return &pb.ResponseForGet{Value: view.GetBytes()}, nil
}
```

实际就是：

```go
GetGroup("scores")
group.Get(ctx, "Tom")
```

因为前面 A 的 Put 已经同步到了 B，所以 B 本地缓存命中：

```text
B.scores["Tom"] = "630"
```

B 返回：

```go
&pb.ResponseForGet{Value: []byte("630")}
```

C 的 `Client.Get` 收到响应：

```go
return resp.GetValue(), nil
```

然后 C 的 `getFromPeer` 返回：

```go
ByteView{bytes: []byte("630")}
```

C 的 `load` 还会把这个远端拿到的值写回 C 本地缓存：

```go
g.mainCache.Add(key, view)
```

所以最终 C 本地也缓存了：

```text
group "scores"
key   "Tom"
value "630"
```

## 八、完整链路汇总

### A/B/C 启动

```text
A/B/C 启动
-> NewClientPicker(addr, serviceName)
-> selfAddr 加入本地一致性哈希环
-> 连接 etcd
-> fetchAllServices 拉取 /services/go-cache/*
-> watchServiceChanges 监听 /services/go-cache/*
```

### A/B/C 创建 group

```text
A/B/C 创建 group
-> NewGroup("scores", ..., WithPeers(picker))
-> group.name = "scores"
-> group.peers = 当前节点的 ClientPicker
```

### A/B/C 创建 server

```text
A/B/C 创建 server
-> NewServer(addr, "go-cache")
-> pb.RegisterGoCacheServer(grpcServer, server)
-> server.Start()
-> register.Register("go-cache", addr, stopCh)
-> etcd 写入 /services/go-cache/{addr} = addr
-> grpcServer.Serve(listener)
```

### A 执行 Put("Tom", "630")

```text
A 执行 Put("Tom", "630")
-> A.group.Put
-> A 本地写缓存
-> A.syncToPeers("put", "Tom", "630")
-> A.picker.PickPeer("Tom")
-> 一致性哈希得到 B.addr
-> A.clients[B.addr].Put("scores", "Tom", "630")
-> gRPC 调 B.Server.Put
-> B.GetGroup("scores")
-> B.group.Put(ctx with from_peer, "Tom", "630")
-> B 本地写缓存，不再继续同步
```

### C 执行 Get("Tom")

```text
C 执行 Get("Tom")
-> C.group.Get
-> C 本地没命中
-> C.loadData
-> C.picker.PickPeer("Tom")
-> 一致性哈希得到 B.addr
-> C.clients[B.addr].Get("scores", "Tom")
-> gRPC 调 B.Server.Get
-> B.GetGroup("scores")
-> B.group.Get("Tom")
-> B 本地命中 "630"
-> 返回 ResponseForGet{Value:"630"}
-> C 写回本地缓存
-> 用户拿到 "630"
```

## 九、一句话理解

这个项目的分布式部分可以理解成：

```text
etcd 只负责告诉每个节点“当前有哪些 addr”；
一致性哈希负责把 key 映射到某个 addr；
ClientPicker 负责把 addr 变成 gRPC Client；
Client 负责发 gRPC；
Server 负责接 gRPC 并转回本地 Group；
Group 仍然是所有缓存读写的核心入口。
```
