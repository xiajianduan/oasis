package assets

import (
	_ "embed"
	"os"
	"path/filepath"
	"sync"
)

//go:embed tun2socks_darwin_amd64
var tun2socksData []byte

var (
	extractOnce sync.Once
	extractPath string
	extractErr  error
)

// Tun2socksPath 提取内置的 tun2socks 二进制到临时目录并返回路径（只解压一次）
func Tun2socksPath() (string, error) {
	extractOnce.Do(func() {
		path := filepath.Join(os.TempDir(), "oasis-tun2socks")
		extractErr = os.WriteFile(path, tun2socksData, 0755)
		extractPath = path
	})
	return extractPath, extractErr
}
