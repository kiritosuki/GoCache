package singleflight

import "sync"

// Loader 管理各种请求
type Loader struct {
	mu sync.Mutex
	// 请求集合
	calls map[string]*call
}

// 表示正在进行或者已经结束的请求
type call struct {
	wg    sync.WaitGroup
	value interface{}
	err   error
}

// Do 执行函数 singleFlight机制 如果多次调用 Do() 确保只会执行一次 f
func (d *Loader) Do(key string, f func() (interface{}, error)) (interface{}, error) {
	d.mu.Lock()
	if d.calls == nil {
		d.calls = make(map[string]*call)
	}
	// 如果有正在进行的请求
	if c, ok := d.calls[key]; ok {
		d.mu.Unlock()
		// 阻塞等待上一个请求进行完
		c.wg.Wait()
		return c.value, c.err
	}
	// 如果没有正在进行的请求
	c := &call{}
	c.wg.Add(1)
	d.calls[key] = c
	d.mu.Unlock()
	// 执行真正的函数调用 实际加载数据
	c.value, c.err = f()
	c.wg.Done()
	d.mu.Lock()
	delete(d.calls, key)
	d.mu.Unlock()
	return c.value, c.err
}
