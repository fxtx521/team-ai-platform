package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 路径硬边界测试（AGENTS.md 禁令 7）：逃逸必须 100% 被拦截。

func newTestGuard(t *testing.T) *Guard {
	t.Helper()
	g, err := NewPathGuard(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("NewPathGuard: %v", err)
	}
	return g
}

func TestResolveInsideOK(t *testing.T) {
	g := newTestGuard(t)
	p, err := g.Resolve("a/b/c.txt")
	if err != nil {
		t.Fatalf("合法相对路径被拒: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("应返回绝对路径: %s", p)
	}
}

func TestResolveDotDotEscape(t *testing.T) {
	g := newTestGuard(t)
	if _, err := g.Resolve("../../etc/passwd"); err == nil {
		t.Fatal(".. 穿越未被拦截")
	}
}

func TestResolveAbsoluteOutside(t *testing.T) {
	g := newTestGuard(t)
	if _, err := g.Resolve("/etc/passwd"); err == nil {
		t.Fatal("绝对路径越界未被拦截")
	}
}

func TestResolveSymlinkEscape(t *testing.T) {
	g := newTestGuard(t)
	outside := t.TempDir() // 授权目录之外的真实目录
	// 在授权目录内造一个指向外面的符号链接。
	link := filepath.Join(g.root, "leak")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("setup symlink: %v", err)
	}
	if _, err := g.Resolve("leak/secret.txt"); err == nil {
		t.Fatal("符号链接逃逸未被拦截")
	}
}

func TestWriteReadRoundtrip(t *testing.T) {
	g := newTestGuard(t)
	if err := g.WriteFile("d/x.txt", []byte("hello"), 0o640); err != nil {
		t.Fatalf("写: %v", err)
	}
	b, err := g.ReadFile("d/x.txt")
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("roundtrip 内容不一致: %q", b)
	}
	if _, err := g.ReadFile("../x.txt"); err == nil {
		t.Fatal("读路径越界未被拦截")
	}
}

func TestResolveEmpty(t *testing.T) {
	g := newTestGuard(t)
	if _, err := g.Resolve(""); err == nil {
		t.Fatal("空路径未被拦截")
	}
}
