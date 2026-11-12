package memory_test

import (
	"bytes"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 全仓库 gofmt 检查：这样 `go test ./...` 一个命令就是完整的本地 CI，
// 不需要额外的脚本（go test 本身会跑 vet 的常用子集）。
// 放在哪个包无所谓，用 go.mod 定位仓库根，避免依赖工作目录。
func TestFormat(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir // 跳过 .git 等隐藏目录
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Windows 上 checkout 可能带 CRLF，先归一化再比对，避免明明是格式正确的文件被误报
		src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
		want, err := format.Source(src)
		if err != nil {
			return err
		}
		if !bytes.Equal(want, src) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s 未 gofmt（跑 gofmt -w .）", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// repoRoot 向上找 go.mod。
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
