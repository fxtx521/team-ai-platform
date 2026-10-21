// executor — 员工端本地执行器（Mac/Win 同一代码库）
//
// 宪法（AGENTS.md）：无 UI、无内置模型调用、无本地记忆存储。
// 本文件只做骨架：main 入口 + 工具注册表 + 主循环占位。
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Tool 执行器本地工具接口：服务端大脑下发的指令在这里落地。
type Tool interface {
	Name() string
	// Invoke 执行工具调用。args 是服务端下发的 JSON 参数。
	// 返回 JSON 可序列化结果。
	Invoke(ctx context.Context, args json.RawMessage) (any, error)
}

// registry 工具注册表。W3 逐个补：file_read/file_write/cmd/browser/report。
var registry = map[string]Tool{}

func register(t Tool) {
	registry[t.Name()] = t
}

// Config 运行配置：环境变量注入，安装包内置默认值。
type Config struct {
	ServerURL  string // 公司服务器地址
	APIKey     string // 终端 API Key（不含模型 key，模型 key 永不下发）
	DataDir    string // 授权目录（路径硬边界的根）
	DeviceFP   string // 设备指纹
}

func loadConfig() Config {
	return Config{
		ServerURL: envOr("TAP_SERVER_URL", "https://api.company.internal"),
		APIKey:    os.Getenv("TAP_API_KEY"),
		DataDir:   envOr("TAP_DATA_DIR", "./tap-data"),
		DeviceFP:  envOr("TAP_DEVICE_FP", "unregistered"),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("executor 骨架启动 server=%s fp=%s tools=%d", cfg.ServerURL, cfg.DeviceFP, len(registry))

	// W3 主循环骨架：heartbeat → inbox/pull → 工具执行 → steps 上报。
	// 现在只做周期心跳占位，保证双平台能编译跑起来。
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("收到退出信号，bye")
			return
		case <-ticker.C:
			log.Println("heartbeat tick（W3 实现 HTTP 上报）")
		}
	}
}
