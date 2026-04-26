package goch

// ByteView 只读的字节视图 用于缓存数据
type ByteView struct {
	bytes []byte
}

// Len 获取bytes长度
func (b ByteView) Len() int {
	return len(b.bytes)
}

// GetBytes 获取bytes的拷贝切片
func (b ByteView) GetBytes() []byte {
	return cloneBytes(b.bytes)
}

func cloneBytes(bytes []byte) []byte {
	cloned := make([]byte, len(bytes))
	copy(cloned, bytes)
	return cloned
}

// String 获取字符串
func (b ByteView) String() string {
	return string(b.bytes)
}
