package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kiritosuki/GoCache/goch"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18001", "current gRPC node address")
	service := flag.String("service", goch.DefaultServiceName, "service name registered in etcd")
	groupName := flag.String("group", "scores", "cache group name")
	probeKey := flag.String("probe-key", "", "run one Get through the cache group after startup")
	probeValue := flag.String("probe-value", "", "run one Put through the cache group after startup")
	exitAfterProbe := flag.Bool("exit-after-probe", false, "exit after probe operations")
	flag.Parse()

	ctx := context.Background()
	source := map[string]string{
		"Tom":  "630",
		"Jack": "589",
		"Sam":  "567",
		"key3": "333",
	}

	picker, err := goch.NewClientPicker(*addr, goch.WithServiceName(*service))
	if err != nil {
		panic(err)
	}
	defer picker.Close()

	group := goch.NewGroup(*groupName, 1<<20, goch.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			fmt.Printf("[%s] load %q from data source\n", *addr, key)
			if v, ok := source[key]; ok {
				return []byte(v), nil
			}
			return nil, fmt.Errorf("%s not found", key)
		},
	), goch.WithExpr(5*time.Minute), goch.WithPeers(picker))
	defer group.Close()

	server, err := goch.NewServer(*addr, *service)
	if err != nil {
		panic(err)
	}
	go func() {
		if err := server.Start(); err != nil {
			panic(err)
		}
	}()

	time.Sleep(2 * time.Second)
	if *probeValue != "" {
		if err := group.Put(ctx, *probeKey, []byte(*probeValue)); err != nil {
			panic(err)
		}
		fmt.Printf("[%s] put %s=%s\n", *addr, *probeKey, *probeValue)
	}
	if *probeKey != "" {
		view, err := group.Get(ctx, *probeKey)
		if err != nil {
			panic(err)
		}
		fmt.Printf("[%s] get %s=%s\n", *addr, *probeKey, view.String())
	}
	if *exitAfterProbe {
		return
	}

	fmt.Printf("[%s] GoCache node is running. Press Ctrl+C to stop.\n", *addr)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
}
