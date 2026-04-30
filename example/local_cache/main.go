package main

import (
	"context"
	"fmt"
	"time"

	"github.com/kiritosuki/GoCache/goch"
)

func main() {
	ctx := context.Background()
	db := map[string]string{
		"Tom":  "630",
		"Jack": "589",
		"Sam":  "567",
	}

	group := goch.NewGroup("scores", 1<<20, goch.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			fmt.Printf("load %q from data source\n", key)
			if v, ok := db[key]; ok {
				return []byte(v), nil
			}
			return nil, fmt.Errorf("%s not found", key)
		},
	), goch.WithExpr(time.Minute))
	defer group.Close()

	for i := 0; i < 2; i++ {
		view, err := group.Get(ctx, "Tom")
		if err != nil {
			panic(err)
		}
		fmt.Printf("get Tom: %s\n", view.String())
	}

	if err := group.Put(ctx, "Alice", []byte("711")); err != nil {
		panic(err)
	}
	view, err := group.Get(ctx, "Alice")
	if err != nil {
		panic(err)
	}
	fmt.Printf("get Alice: %s\n", view.String())

	if err := group.Delete(ctx, "Alice"); err != nil {
		panic(err)
	}
	if _, err := group.Get(ctx, "Alice"); err != nil {
		fmt.Printf("get Alice after delete: %v\n", err)
	}

	fmt.Printf("stats: %+v\n", group.Stats())
}
