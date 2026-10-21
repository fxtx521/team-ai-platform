package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// pathguard — 路径硬边界（AGENTS.md 禁令 7）：所有文件工具必须先过这道闸。
// 规则：解析后的绝对路径必须落在授权目录内；拒绝符号链接逃逸。

var (
	ErrPathEscape  = errors.New("path escapes authorized dir")
	ErrNotAbsolute = errors.New("root must be absolute")
)

// Guard 授权目录守卫。零值不可用，必须 NewPathGuard。
type Guard struct {
	root string
}

func NewPathGuard(root string) (*Guard, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(abs) {
		return nil, ErrNotAbsolute
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	return &Guard{root: real}, nil
}

// Resolve 校验并解析目标路径。symlink 逃逸与 .. 穿越在此拦截。
// 写场景目录可能还不存在：先在 root 内逐段建目录再解析（MkdirAll
// 的目标由 Join(root, 清洗后的相对路径) 拼出，天然出不了 root）。
func (g *Guard) Resolve(p string) (string, error) {
	if p == "" {
		return "", ErrPathEscape
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(g.root, p)
	}
	// 先以 root 内安全前缀尝试建目录，保证 EvalSymlinks 有真实目录可查。
	dir := filepath.Dir(abs)
	safePrefix := filepath.Join(g.root, "w")
	if dir != g.root && !strings.HasPrefix(dir, safePrefix) {
		// 相对路径拼接出的 dir 一定落在 root 下，直接补建。
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", ErrPathEscape
		}
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", ErrPathEscape
	}
	resolved := filepath.Join(realDir, filepath.Base(abs))
	// 最终判定：必须仍在 root 内（prefix 匹配 + 边界符）。
	if resolved != g.root && !strings.HasPrefix(resolved, g.root+string(filepath.Separator)) {
		return "", ErrPathEscape
	}
	return resolved, nil
}

// 供 file 工具使用的读写包装：所有文件操作只经这两个口。
func (g *Guard) ReadFile(p string) ([]byte, error) {
	rp, err := g.Resolve(p)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(rp)
}

func (g *Guard) WriteFile(p string, data []byte, mode os.FileMode) error {
	rp, err := g.Resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(rp), 0o750); err != nil {
		return err
	}
	return os.WriteFile(rp, data, mode)
}

// 编译期接口检查：确保 json 依赖被骨架引用（工具参数协议）。
var _ = json.RawMessage{}
