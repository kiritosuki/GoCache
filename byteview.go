package GoCache

// ByteView 只读的字节视图 用于缓存数据
type ByteView struct {
	bytes []byte
}

// Len 获取bytes长度
func (b *ByteView) Len() int {
	return len(b.bytes)
}

// GetBytes 获取bytes的拷贝切片
func (b *ByteView) GetBytes() []byte {
	cloned := make([]byte, len(b.bytes))
	copy(cloned, b.bytes)
	return cloned
}
