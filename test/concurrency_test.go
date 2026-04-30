package test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kiritosuki/GoCache/goch"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const etcdEndpoint = "127.0.0.1:2379"

func TestConcurrentLocalGroupOperations(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var loads int64
	groupName := fmt.Sprintf("local-concurrency-%d", time.Now().UnixNano())
	group := goch.NewGroup(groupName, 8<<20, goch.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			atomic.AddInt64(&loads, 1)
			return []byte("source-" + key), nil
		},
	), goch.WithExpr(2*time.Second))
	defer group.Close()

	runConcurrent(t, 96, 300, func(worker, iter int) {
		key := fmt.Sprintf("key-%03d", (worker+iter)%128)
		switch iter % 5 {
		case 0:
			if err := group.Put(ctx, key, []byte(fmt.Sprintf("put-%d-%d", worker, iter))); err != nil {
				t.Errorf("Put(%s): %v", key, err)
			}
		case 1:
			if err := group.Delete(ctx, key); err != nil {
				t.Errorf("Delete(%s): %v", key, err)
			}
		case 2:
			group.Clear()
		case 3:
			_ = group.Stats()
		default:
			if _, err := group.Get(ctx, key); err != nil {
				t.Errorf("Get(%s): %v", key, err)
			}
		}
	})

	if got := atomic.LoadInt64(&loads); got == 0 {
		t.Fatal("expected at least one loader call")
	}
}

func TestConcurrentDistributedGroupOperations(t *testing.T) {
	if os.Getenv("GOCACHE_DISTRIBUTED_NODE") == "1" {
		runDistributedNodeHelper(t)
		return
	}

	if testing.Short() {
		t.Skip("skipping distributed concurrency test in short mode")
	}
	etcd := newEtcdClientOrSkip(t)
	defer etcd.Close()

	ctx := context.Background()
	serviceName := fmt.Sprintf("go-cache-concurrency-%d", time.Now().UnixNano())
	groupName := fmt.Sprintf("distributed-concurrency-%d", time.Now().UnixNano())
	cleanupService(t, etcd, serviceName)
	defer cleanupService(t, etcd, serviceName)

	addrs := []string{freeTCPAddr(t), freeTCPAddr(t), freeTCPAddr(t)}
	procs := make([]*exec.Cmd, 0, len(addrs))
	for _, addr := range addrs {
		cmd := startDistributedNodeProcess(t, addr, serviceName, groupName)
		procs = append(procs, cmd)
	}
	defer func() {
		for _, cmd := range procs {
			stopProcess(cmd)
		}
	}()

	waitForServiceCount(t, etcd, serviceName, len(addrs), 10*time.Second)

	clientAddr := freeTCPAddr(t)
	picker, err := goch.NewClientPicker(clientAddr, goch.WithServiceName(serviceName))
	if err != nil {
		t.Fatalf("NewClientPicker: %v", err)
	}
	defer picker.Close()

	group := goch.NewGroup(groupName, 8<<20, goch.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			return []byte("client-source-" + key), nil
		},
	), goch.WithExpr(5*time.Second), goch.WithPeers(picker))
	defer group.Close()

	time.Sleep(2 * time.Second)
	runConcurrent(t, 72, 180, func(worker, iter int) {
		key := fmt.Sprintf("key-%03d", (worker*31+iter)%256)
		switch iter % 4 {
		case 0:
			if err := group.Put(ctx, key, []byte(fmt.Sprintf("value-%d-%d", worker, iter))); err != nil {
				t.Errorf("Put(%s): %v", key, err)
			}
		case 1:
			if err := group.Delete(ctx, key); err != nil {
				t.Errorf("Delete(%s): %v", key, err)
			}
		case 2:
			_ = group.Stats()
		default:
			view, err := group.Get(ctx, key)
			if err != nil {
				t.Errorf("Get(%s): %v", key, err)
				return
			}
			if view.Len() == 0 {
				t.Errorf("Get(%s): empty value", key)
			}
		}
	})

	time.Sleep(500 * time.Millisecond)
}

func runConcurrent(t *testing.T, workers int, iterations int, fn func(worker, iter int)) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iter := 0; iter < iterations; iter++ {
				fn(worker, iter)
			}
		}(worker)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("concurrent workload timed out: workers=%d iterations=%d", workers, iterations)
	}
}

func runDistributedNodeHelper(t *testing.T) {
	t.Helper()
	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep == -1 || len(args) < sep+4 {
		t.Fatalf("missing helper args")
	}
	addr := args[sep+1]
	serviceName := args[sep+2]
	groupName := args[sep+3]

	picker, err := goch.NewClientPicker(addr, goch.WithServiceName(serviceName))
	if err != nil {
		t.Fatalf("helper NewClientPicker: %v", err)
	}
	defer picker.Close()

	group := goch.NewGroup(groupName, 8<<20, goch.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			return []byte("node-source-" + addr + "-" + key), nil
		},
	), goch.WithExpr(5*time.Second), goch.WithPeers(picker))
	defer group.Close()

	server, err := goch.NewServer(addr, serviceName, goch.WithEtcdEndpoints([]string{etcdEndpoint}))
	if err != nil {
		t.Fatalf("helper NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-stop:
		_ = sig
	case err := <-errCh:
		if err != nil {
			t.Fatalf("helper server stopped: %v", err)
		}
	}
}

func newEtcdClientOrSkip(t *testing.T) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdEndpoint},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Skipf("etcd unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cli.Get(ctx, "health-check"); err != nil {
		cli.Close()
		t.Skipf("etcd unavailable: %v", err)
	}
	return cli
}

func cleanupService(t *testing.T, cli *clientv3.Client, serviceName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Delete(ctx, "/services/"+serviceName, clientv3.WithPrefix()); err != nil {
		t.Fatalf("cleanup service %s: %v", serviceName, err)
	}
}

func waitForServiceCount(t *testing.T, cli *clientv3.Client, serviceName string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		resp, err := cli.Get(ctx, "/services/"+serviceName, clientv3.WithPrefix())
		cancel()
		if err == nil {
			last = len(resp.Kvs)
			if last >= want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d service registrations, got %d", want, last)
}

func startDistributedNodeProcess(t *testing.T, addr string, serviceName string, groupName string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run=TestConcurrentDistributedGroupOperations",
		"--",
		addr,
		serviceName,
		groupName,
	)
	cmd.Env = append(os.Environ(), "GOCACHE_DISTRIBUTED_NODE=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node %s: %v", addr, err)
	}
	return cmd
}

func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	case err := <-done:
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			if exitErr, ok := err.(*exec.ExitError); ok && strings.Contains(exitErr.Error(), "signal: terminated") {
				return
			}
		}
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate tcp port: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
}
