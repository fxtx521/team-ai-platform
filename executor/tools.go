// tools.go — 执行器本地工具集：文件读、文件写、命令执行。
//
// 宪法（AGENTS.md）：本地能力是"手"，不带"脑"——这里不做任何模型调用，
// 文件操作一律先过 pathguard 的硬边界（禁令 7）。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// cmdTimeout 单条命令硬超时：本地命令必须有上界，一条 sleep 不能把常驻进程吊死。
	cmdTimeout = 60 * time.Second

	// cmdOutputLimit 单条命令 stdout/stderr 各自最多留 1 MiB，超出截断并标记，
	// 防止一条喷日志的命令把执行器内存打满。
	cmdOutputLimit = 1 << 20

	// fileMode 文件写入权限 0640：本人可写、同组只读、其他人无权限，符合企业内网默认。
	fileMode = 0o640
)

// ToolError 结构化工具失败：带上退出码，让上报层能区分"工具跑失败"与"执行器出事"。
type ToolError struct {
	Msg      string
	ExitCode int
}

func (e *ToolError) Error() string { return e.Msg }

// registerTools 把所有本地工具挂进 registry；注册表就是"这台执行器能干什么"的唯一清单。
func registerTools(guard *Guard) {
	register(&fileReadTool{guard: guard})
	register(&fileWriteTool{guard: guard})
	register(&cmdTool{})
}

// ---------------------------------------------------------------------------
// file_read
// ---------------------------------------------------------------------------

// fileReadTool 读授权目录内的文件；读也过 Guard，边界不分读写。
type fileReadTool struct{ guard *Guard }

func (t *fileReadTool) Name() string { return "file_read" }

type fileReadArgs struct {
	Path string `json:"path"`
}

// fileResult 文件读结果的统一形状：内容用 b64 保证二进制安全，
// 同时给 sha256 和大小，服务端可据此登记产物而不必回传内容。
type fileResult struct {
	Path       string `json:"path"`
	SizeBytes  int    `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	ContentB64 string `json:"content_b64"`
	Content    string `json:"content,omitempty"` // 仅当内容是合法 UTF-8 才给，方便人看日志
}

func (t *fileReadTool) Invoke(_ context.Context, args json.RawMessage) (any, error) {
	var a fileReadArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("file_read 参数非法: %w", err)
	}
	data, err := t.guard.ReadFile(a.Path)
	if err != nil {
		return nil, fmt.Errorf("file_read %q: %w", a.Path, err)
	}
	return newFileResult(a.Path, data), nil
}

// newFileResult 抹平"读来的"和"刚写的"两种来源的结果形状，上报端不用分情况。
func newFileResult(path string, data []byte) fileResult {
	sum := sha256.Sum256(data)
	res := fileResult{
		Path:       path,
		SizeBytes:  len(data),
		SHA256:     hex.EncodeToString(sum[:]),
		ContentB64: base64.StdEncoding.EncodeToString(data),
	}
	if utf8.Valid(data) {
		res.Content = string(data)
	}
	return res
}

// ---------------------------------------------------------------------------
// file_write
// ---------------------------------------------------------------------------

// fileWriteTool 写授权目录内的文件；路径先过 Guard，再解码内容，最后落盘。
type fileWriteTool struct{ guard *Guard }

func (t *fileWriteTool) Name() string { return "file_write" }

type fileWriteArgs struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

type fileWriteResult struct {
	Path      string `json:"path"` // 落盘绝对路径，服务端核对产物位置用
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Mode      string `json:"mode"`
}

func (t *fileWriteTool) Invoke(_ context.Context, args json.RawMessage) (any, error) {
	var a fileWriteArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("file_write 参数非法: %w", err)
	}
	data, err := decodeB64(a.ContentB64)
	if err != nil {
		return nil, err
	}
	// 先解析路径再写：解析失败就直接拒，绝不"先写到临时位置再看看"。
	abs, err := t.guard.Resolve(a.Path)
	if err != nil {
		return nil, fmt.Errorf("file_write %q: %w", a.Path, err)
	}
	if err := t.guard.WriteFile(a.Path, data, fileMode); err != nil {
		return nil, fmt.Errorf("file_write %q: %w", a.Path, err)
	}
	sum := sha256.Sum256(data)
	return fileWriteResult{
		Path:      abs,
		SizeBytes: len(data),
		SHA256:    hex.EncodeToString(sum[:]),
		Mode:      "0640",
	}, nil
}

// decodeB64 容忍标准/URL 安全两种编码与换行、空格：脚本或服务端产出的 b64 常带换行，
// 直接 StdEncoding 解码会失败；这里按"内容是 base64"的本意尽量解开，解不开才报错。
func decodeB64(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', ' ':
			return -1
		}
		return r
	}, s)
	var errs []error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if data, err := enc.DecodeString(clean); err == nil {
			return data, nil
		} else {
			errs = append(errs, err)
		}
	}
	return nil, fmt.Errorf("content_b64 解码失败: %w", errors.Join(errs...))
}

// ---------------------------------------------------------------------------
// cmd
// ---------------------------------------------------------------------------

// cmdTool 跑本机命令。只走 argv 形式（exec.Command），绝不拼 shell 字符串：
// 参数里的 ; | $() ` 永远是字面量，注入面为零。
type cmdTool struct{}

func (t *cmdTool) Name() string { return "cmd" }

type cmdArgs struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type cmdResult struct {
	Command         string   `json:"command"`
	Args            []string `json:"args"`
	ExitCode        int      `json:"exit_code"`
	Stdout          string   `json:"stdout"`
	Stderr          string   `json:"stderr"`
	DurationMS      int64    `json:"duration_ms"`
	StdoutTruncated bool     `json:"stdout_truncated,omitempty"`
	StderrTruncated bool     `json:"stderr_truncated,omitempty"`
}

// shellEntrypoints argv 形式已杜绝注入，但把 shell 当入口等于把注入面重新打开
// （sh -c "..." 就是把拼接塞回来），所以显式拒绝。
var shellEntrypoints = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true, "csh": true, "tcsh": true,
	"fish": true, "cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true, "pwsh": true,
}

func (t *cmdTool) Invoke(ctx context.Context, args json.RawMessage) (any, error) {
	var a cmdArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("cmd 参数非法: %w", err)
	}
	command := strings.TrimSpace(a.Command)
	if command == "" {
		return nil, errors.New("cmd: command 不能为空")
	}
	if base := strings.ToLower(filepath.Base(command)); shellEntrypoints[base] {
		return nil, fmt.Errorf("cmd: 拒绝以 shell 作为入口 (%s)，请直接给可执行文件与参数", base)
	}

	// 上下文超时覆盖整棵树：exec.CommandContext 会杀进程，不会留下跑飞的子进程。
	runCtx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()

	outBuf := &cappedBuffer{limit: cmdOutputLimit}
	errBuf := &cappedBuffer{limit: cmdOutputLimit}
	cmd := exec.CommandContext(runCtx, command, a.Args...)
	cmd.Stdout, cmd.Stderr = outBuf, errBuf

	started := time.Now()
	runErr := cmd.Run()
	res := cmdResult{
		Command:         command,
		Args:            a.Args,
		Stdout:          outBuf.String(),
		Stderr:          errBuf.String(),
		DurationMS:      time.Since(started).Milliseconds(),
		StdoutTruncated: outBuf.Truncated(),
		StderrTruncated: errBuf.Truncated(),
	}
	if runErr == nil {
		return res, nil
	}
	// 超时优先判：被杀掉的进程同时是 ExitError，先认超时才能给对原因。
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.ExitCode = -1
		return res, &ToolError{Msg: fmt.Sprintf("cmd %s 超时(%s)", command, cmdTimeout), ExitCode: -1}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, &ToolError{
			Msg:      fmt.Sprintf("cmd %s 退出码 %d: %s", command, res.ExitCode, firstLine(res.Stderr)),
			ExitCode: res.ExitCode,
		}
	}
	return res, fmt.Errorf("cmd %s 启动失败: %w", command, runErr)
}

// firstLine 失败原因只取 stderr 第一行：上报体不该被一整段日志撑爆。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(无 stderr 输出)"
	}
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

// cappedBuffer 只保留前 limit 字节，写满后丢弃并置截断标记。
// Write 永远返回 len(p), nil：写短了会让子进程因 EPIPE 提前死掉，反而丢诊断信息。
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.limit - b.buf.Len()
	switch {
	case room <= 0:
		if len(p) > 0 {
			b.truncated = true
		}
	case len(p) > room:
		_, _ = b.buf.Write(p[:room])
		b.truncated = true
	default:
		_, _ = b.buf.Write(p)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *cappedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
