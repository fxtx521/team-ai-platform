// executor — 员工端本地执行器（Mac/Win 同一代码库）
//
// 宪法（AGENTS.md）：无 UI、无内置模型调用、无本地记忆存储。
// 本文件只做装配：main 入口 + 工具注册表 + 主循环（心跳 → 拉活 → 执行 → 上报）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// agentVersion 心跳上报的版本号；发版时同步改，服务端据此判断终端能力。
const agentVersion = "0.3.0"

// defaultPollInterval 一轮 30s：心跳与拉活同频，够快又不至于把控制面打满。
const defaultPollInterval = 30 * time.Second

// Tool 执行器本地工具接口：服务端大脑下发的指令在这里落地。
type Tool interface {
	Name() string
	// Invoke 执行工具调用。args 是服务端下发的 JSON 参数。
	// 返回 JSON 可序列化结果。
	Invoke(ctx context.Context, args json.RawMessage) (any, error)
}

// registry 工具注册表。W3 已挂 file_read/file_write/cmd。
var registry = map[string]Tool{}

func register(t Tool) {
	registry[t.Name()] = t
}

// toolNames 注册表快照，启动日志与错误里打出来，排障时一眼看出这版执行器带了哪些工具。
func toolNames() string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// inflight 已拉到手、还没上报完的任务数，心跳的 queue_len 就报它——
// 服务端据此判断这台机器是否还压着活，不掺假数据。
var inflight atomic.Int64

// Config 运行配置：环境变量注入，安装包内置默认值。
type Config struct {
	ServerURL    string        // 公司服务器地址
	APIKey       string        // 终端 API Key（不含模型 key，模型 key 永不下发）
	DataDir      string        // 授权目录（路径硬边界的根）
	DeviceFP     string        // 设备指纹
	PollInterval time.Duration // 心跳+拉活间隔
}

func loadConfig() Config {
	poll := defaultPollInterval
	if v := os.Getenv("TAP_POLL_INTERVAL"); v != "" {
		// 冒烟脚本要把 30s 的等待压到秒级，所以间隔可注入；值非法就退回默认，
		// 配置写错不该让执行器起不来。
		if d, err := time.ParseDuration(v); err == nil && d >= time.Second {
			poll = d
		} else {
			log.Printf("TAP_POLL_INTERVAL=%q 非法，退回 %s", v, defaultPollInterval)
		}
	}
	return Config{
		ServerURL:    envOr("TAP_SERVER_URL", "https://api.company.internal"),
		APIKey:       envOr("TAP_API_KEY", ""),
		DataDir:      envOr("TAP_DATA_DIR", "./tap-data"),
		DeviceFP:     envOr("TAP_DEVICE_FP", "unregistered"),
		PollInterval: poll,
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

	// 授权目录必须先立起来：文件工具没有边界，还不如没有文件工具。
	guard, err := NewPathGuard(cfg.DataDir)
	if err != nil {
		log.Printf("授权目录不可用 (%s): %v", cfg.DataDir, err)
		os.Exit(1)
	}
	registerTools(guard)

	client := NewHTTPClient(cfg.ServerURL, cfg.APIKey, cfg.DeviceFP)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 没有 token 什么也做不了：认证失败按退出码 1 退出，让安装器/守护进程立刻重试。
	if err := client.Auth(ctx); err != nil {
		log.Printf("认证失败 server=%s fp=%s: %v", cfg.ServerURL, cfg.DeviceFP, err)
		stop()
		os.Exit(1)
	}
	log.Printf("executor 启动 version=%s server=%s terminal=%s user=%s data=%s tools=%s poll=%s",
		agentVersion, cfg.ServerURL, client.TerminalID(), client.UserID(), cfg.DataDir, toolNames(), cfg.PollInterval)

	runLoop(ctx, client, cfg.PollInterval)
	log.Println("收到退出信号，bye")
}

// runLoop 一轮 = 心跳 + 拉 inbox + 逐任务执行上报。先跑一轮再等 ticker：
// 执行器晚于任务启动时不必干等一个周期。
func runLoop(ctx context.Context, client *HTTPClient, poll time.Duration) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	cycle(ctx, client)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle(ctx, client)
		}
	}
}

// cycle 单轮工作。任何一步失败只记日志不退出：常驻进程要能自己缓过来。
func cycle(ctx context.Context, client *HTTPClient) {
	if err := client.Heartbeat(ctx, int(inflight.Load())); err != nil {
		log.Printf("心跳失败: %v", err)
	}
	tasks, err := client.PullInbox(ctx)
	if err != nil {
		log.Printf("拉 inbox 失败: %v", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	log.Printf("拉到 %d 个任务", len(tasks))
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		inflight.Add(1)
		runTask(ctx, client, task)
		inflight.Add(-1)
	}
}

// runTask 执行一个任务的全部步骤并整批上报。前一步失败就停：后面的步骤通常依赖
// 前一步的产物，硬跑只会制造一串假故障。
func runTask(ctx context.Context, client *HTTPClient, task InboxTask) {
	log.Printf("任务 %s 开始 title=%q type=%s", task.ID, task.Title, task.Type)

	specs, err := plannedSteps(task)
	if err != nil {
		// 拉到了活却解不出指令，也要上报一条失败步骤：否则服务端永远看到 running。
		failed := reportStep{Tool: "task.plan", StartedAt: isoNow(), FinishedAt: isoNow(), Error: err.Error()}
		reportSteps(ctx, client, task.ID, []reportStep{failed}, "failed")
		return
	}

	steps := make([]reportStep, 0, len(specs))
	failed := false
	for i, spec := range specs {
		step := executeStep(ctx, i+1, spec)
		steps = append(steps, step)
		if !step.OK {
			failed = true
			break
		}
	}
	status := "done"
	if failed {
		status = "failed"
	}
	reportSteps(ctx, client, task.ID, steps, status)
}

// reportSteps 上报并把结果打到日志里；上报失败下轮幂等重发由 request_id 兜住。
func reportSteps(ctx context.Context, client *HTTPClient, taskID string, steps []reportStep, status string) {
	if err := client.ReportSteps(ctx, taskID, steps, status); err != nil {
		log.Printf("任务 %s 步骤上报失败 (status=%s): %v", taskID, status, err)
		return
	}
	log.Printf("任务 %s 已上报 status=%s steps=%d", taskID, status, len(steps))
}

// stepSpec 服务端下发的单条工具指令：{"tool":"file_write","args":{...}}。
type stepSpec struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// executeStep 查注册表执行一条指令并组装上报步骤。工具报错只影响本步骤，
// 执行器本身永远不 panic。
func executeStep(ctx context.Context, seq int, spec stepSpec) reportStep {
	step := reportStep{Seq: seq, Tool: spec.Tool, StartedAt: isoNow()}
	tool, ok := registry[spec.Tool]
	if !ok {
		step.FinishedAt = isoNow()
		step.Error = fmt.Sprintf("工具未注册: %q（本机可用: %s）", spec.Tool, toolNames())
		return step
	}
	result, err := tool.Invoke(ctx, spec.Args)
	step.FinishedAt = isoNow()
	if result != nil {
		// 契约 steps 报文没有 result 字段，结果只在本地日志留痕；产物登记走 artifacts。
		if raw, mErr := json.Marshal(result); mErr == nil {
			log.Printf("步骤 %d %s 结果: %s", seq, spec.Tool, truncateLog(string(raw), 400))
		}
	}
	if err != nil {
		step.Error = err.Error()
		var toolErr *ToolError
		if errors.As(err, &toolErr) {
			code := toolErr.ExitCode
			step.ExitCode = &code
		}
		log.Printf("步骤 %d %s 失败: %v", seq, spec.Tool, err)
		return step
	}
	step.OK = true
	return step
}

// plannedSteps 从任务体解出要执行的工具指令。
//
// 契约里任务只落 title/summary（任务原文一律不落盘，硬禁令 4），没有专门的指令
// 字段。所以按 steps → summary → title 顺序探测：服务端大脑将来直接下发 steps
// 字段时执行器不用改；现在用 POST /v1/tasks 建的任务把指令 JSON 塞在 summary 里。
func plannedSteps(task InboxTask) ([]stepSpec, error) {
	type candidate struct {
		field string
		raw   json.RawMessage
	}
	candidates := []candidate{{"steps", task.Steps}}
	if task.Summary != nil {
		candidates = append(candidates, candidate{"summary", json.RawMessage(*task.Summary)})
	}
	candidates = append(candidates, candidate{"title", json.RawMessage(task.Title)})

	for _, c := range candidates {
		specs := parseStepSpecs(c.raw)
		if len(specs) > 0 {
			log.Printf("任务 %s 指令来自 %s，共 %d 步", task.ID, c.field, len(specs))
			return specs, nil
		}
	}
	return nil, errors.New("任务体里没有可解析的 steps 指令（steps/summary/title 都不是 JSON 指令数组）")
}

// parseStepSpecs 宽容解析：接受裸数组、{"steps":[...]}、单个指令对象三种写法。
// 解不动就返回空切片交给上层继续探测别的字段，不返回错误——"这个字段不是指令"是常态。
func parseStepSpecs(raw json.RawMessage) []stepSpec {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || (trimmed[0] != '[' && trimmed[0] != '{') {
		return nil
	}
	if trimmed[0] == '[' {
		var specs []stepSpec
		if err := json.Unmarshal([]byte(trimmed), &specs); err != nil {
			return nil
		}
		return validSpecs(specs)
	}
	var wrapper struct {
		Steps []stepSpec `json:"steps"`
		Tool  string     `json:"tool"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapper); err != nil {
		return nil
	}
	if len(wrapper.Steps) > 0 {
		return validSpecs(wrapper.Steps)
	}
	if wrapper.Tool != "" {
		var single stepSpec
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil
		}
		return validSpecs([]stepSpec{single})
	}
	return nil
}

// validSpecs 丢掉没写 tool 的指令并把缺省 args 补成 {}：宁可少跑一步，
// 也不让一条残缺指令变成"工具未注册"的噪音。
func validSpecs(specs []stepSpec) []stepSpec {
	out := make([]stepSpec, 0, len(specs))
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Tool)
		if name == "" {
			continue
		}
		spec.Tool = name
		if strings.TrimSpace(string(spec.Args)) == "" {
			spec.Args = json.RawMessage("{}")
		}
		out = append(out, spec)
	}
	return out
}

// truncateLog 日志截断：工具结果可能很大，日志只留够排查的头一段。
func truncateLog(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…(截断)"
}
