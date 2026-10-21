// client.go — 执行器与服务端之间唯一的通信口（契约即法律，AGENTS.md）。
//
// 契约 openapi.yaml 的 servers.url 已经含 /v1，而 TAP_SERVER_URL 只配到 host:port，
// 所以路径常量自己带 /v1 前缀：两端各拼一次前缀就会变成 /v1/v1，这里只有一个地方拼。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	apiPrefix     = "/v1"
	pathAuthToken = apiPrefix + "/auth/token"
	pathHeartbeat = apiPrefix + "/heartbeat"
	pathInboxPull = apiPrefix + "/inbox/pull"
	pathTasks     = apiPrefix + "/tasks"

	// maxBodyBytes 响应体上限（4 MiB）：服务端是自家控制面，正常报文远小于此，
	// 设上限只为避免坏响应把执行器内存吃掉。
	maxBodyBytes = 4 << 20

	// tokenRefreshLeeway 提前 60s 续期：避免"刚验完就过期"导致业务请求白跑一次。
	tokenRefreshLeeway = 60 * time.Second

	// 错误码取自 contracts/schema.py ERROR_CODES，禁止在实现里另起数字。
	codeAuthFailed     = 40101
	codeTokenExpired   = 40102
	defaultTokenTTL    = 12 * time.Hour
	headerAuthScheme   = "Bearer "
	defaultHTTPTimeout = 20 * time.Second
)

// APIError 服务端业务错误：把契约错误码解出来，调用方能按码分支而不是靠字符串匹配。
type APIError struct {
	Status  int    // HTTP 状态码
	Code    int    // 契约错误码，缺省 0 表示服务端没按契约返回
	Name    string // 契约 code_name，如 AUTH_FAILED
	Message string // 人话原因
	Body    string // 原始响应体，排查用（已 TrimSpace）
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("服务端 %d (code=%d %s): %s", e.Status, e.Code, e.Name, e.Message)
	}
	return fmt.Sprintf("服务端 %d: %s", e.Status, e.Body)
}

// needsReauth 只有 token 类错误才值得重认证：其它 4xx/5xx 重认证纯属噪音。
func (e *APIError) needsReauth() bool {
	switch {
	case e.Code == codeAuthFailed || e.Code == codeTokenExpired:
		return true
	case e.Status == http.StatusUnauthorized:
		// 服务端没给契约错误码时按 401 兜底，语义上仍是 token 不可用。
		return true
	default:
		return false
	}
}

// TokenError key/指纹本身无效：重认证也救不了，直接上抛让 main 退出码 1。
var ErrAuthRejected = errors.New("认证被拒")

// HTTPClient 带内存 token 的 HTTP 客户端。并发安全：main 单 goroutine 用，
// 但锁住 token 状态是为了将来加并发工具时不埋雷。
type HTTPClient struct {
	baseURL  string
	apiKey   string
	deviceFP string
	hc       *http.Client

	mu         sync.Mutex
	token      string
	expiresAt  time.Time
	userID     string
	terminalID string
}

// NewHTTPClient 构造客户端；serverURL 允许带或不带结尾的 /v1，统一剥掉再拼。
func NewHTTPClient(serverURL, apiKey, deviceFP string) *HTTPClient {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	base = strings.TrimSuffix(base, apiPrefix)
	return &HTTPClient{
		baseURL:  base,
		apiKey:   apiKey,
		deviceFP: deviceFP,
		hc:       &http.Client{Timeout: defaultHTTPTimeout},
	}
}

// TerminalID 认证后服务端分配的终端 id，日志与排障要用。
func (c *HTTPClient) TerminalID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminalID
}

// UserID 认证后服务端认出的人，审计对账要用。
func (c *HTTPClient) UserID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID
}

// Auth 用 api_key + 设备指纹换短期 token；双因子缺一不可，所以两个字段都必填。
func (c *HTTPClient) Auth(ctx context.Context) error {
	payload := map[string]string{
		"api_key":            c.apiKey,
		"device_fingerprint": c.deviceFP,
	}
	var out struct {
		Token      string `json:"token"`
		ExpiresAt  string `json:"expires_at"`
		UserID     string `json:"user_id"`
		TerminalID string `json:"terminal_id"`
	}
	if err := c.doOnce(ctx, http.MethodPost, pathAuthToken, payload, "", &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.needsReauth() {
			return fmt.Errorf("%w: %v", ErrAuthRejected, apiErr)
		}
		return fmt.Errorf("换取 token: %w", err)
	}
	if out.Token == "" {
		return errors.New("换取 token: 服务端未返回 token")
	}
	// expires_at 解析失败就按默认 TTL 兜底，不能让一个格式问题变成每个 tick 都重认证。
	expires := time.Now().Add(defaultTokenTTL)
	if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
		expires = t
	}
	c.mu.Lock()
	c.token, c.expiresAt, c.userID, c.terminalID = out.Token, expires, out.UserID, out.TerminalID
	c.mu.Unlock()
	return nil
}

// Do 带 token 的请求；token 过期或服务端回 40101 时重认证一次再试。
// 网络错误只返回 error，绝 panic（执行器是常驻进程，一次抖动不该让它躺下）。
func (c *HTTPClient) Do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	token, err := c.currentToken(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, method, path, body, token)
	if err == nil {
		return raw, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.needsReauth() {
		return nil, err
	}
	// 只重试一次就够：重认证后还 401，说明 key 或指纹真有问题，再试也是浪费。
	if authErr := c.Auth(ctx); authErr != nil {
		return nil, fmt.Errorf("%w（重认证失败: %v）", err, authErr)
	}
	token, err = c.currentToken(ctx)
	if err != nil {
		return nil, err
	}
	return c.call(ctx, method, path, body, token)
}

// Heartbeat 报在线事实与队列长度；服务端据此判断这台机器能不能派活。
func (c *HTTPClient) Heartbeat(ctx context.Context, queueLen int) error {
	payload := map[string]any{
		"version":   agentVersion,
		"queue_len": queueLen,
	}
	_, err := c.Do(ctx, http.MethodPost, pathHeartbeat, payload)
	return err
}

// InboxTask inbox 拉回的任务。服务端只落标题与摘要（硬禁令 4），
// 所以指令要从 Steps/Summary 里解，见 plannedSteps。
type InboxTask struct {
	ID         string          `json:"id"`
	RequestID  string          `json:"request_id"`
	UserID     string          `json:"user_id"`
	TerminalID string          `json:"terminal_id"`
	Title      string          `json:"title"`
	Type       string          `json:"type"`
	Source     string          `json:"source"`
	Status     string          `json:"status"`
	Summary    *string         `json:"summary"`
	Steps      json.RawMessage `json:"steps"` // 服务端将来直接下发指令时用；现在恒为 nil
}

// PullInbox 拉取待执行任务；服务端拉走即置 running 并绑定本终端。
func (c *HTTPClient) PullInbox(ctx context.Context) ([]InboxTask, error) {
	raw, err := c.Do(ctx, http.MethodGet, pathInboxPull, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks    []InboxTask `json:"tasks"`
		Count    int         `json:"count"`
		PulledAt string      `json:"pulled_at"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 inbox 响应: %w", err)
	}
	return resp.Tasks, nil
}

// reportStep 步骤上报报文。契约 STEP_SCHEMA_REQUIRED 只强制 tool/ok/started_at；
// exit_code 契约里没有（服务端 DTO 会丢弃），但保留在报文里无害，且失败原因已同步
// 写进 error，信息不会在两端之间丢失。
type reportStep struct {
	Seq        int    `json:"seq,omitempty"`
	Tool       string `json:"tool"`
	OK         bool   `json:"ok"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
}

// ReportSteps 整批上报一次任务的全部步骤；request_id 幂等，断线重发不会重复计数。
func (c *HTTPClient) ReportSteps(ctx context.Context, taskID string, steps []reportStep, status string) error {
	body := map[string]any{
		"request_id": reportRequestID(taskID),
		"steps":      steps,
	}
	if status != "" {
		body["status"] = status
	}
	_, err := c.Do(ctx, http.MethodPost, pathTaskSteps(taskID), body)
	return err
}

// reportRequestID 用任务 id 派生幂等键：同一任务重发同一批步骤时服务端按
// (task_id, request_id) 整批重放，计数与状态都不会翻倍。
func reportRequestID(taskID string) string { return "exec-" + taskID }

// pathTaskSteps 任务 id 来自服务端，仍按路径段转义，避免拼接出别的端点。
func pathTaskSteps(taskID string) string {
	return pathTasks + "/" + url.PathEscape(taskID) + "/steps"
}

// currentToken 取可用 token，没有或快过期就先认证。
func (c *HTTPClient) currentToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	token, expires := c.token, c.expiresAt
	c.mu.Unlock()
	if token != "" && time.Now().Add(tokenRefreshLeeway).Before(expires) {
		return token, nil
	}
	if err := c.Auth(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	token = c.token
	c.mu.Unlock()
	return token, nil
}

// call 单次请求，token 由调用方给（便于重认证后换新 token 重放同一请求体）。
func (c *HTTPClient) call(ctx context.Context, method, path string, body any, token string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.doOnce(ctx, method, path, body, token, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// doOnce 编请求、发请求、解错误码。所有 HTTP 细节收敛在这一个函数里。
func (c *HTTPClient) doOnce(ctx context.Context, method, path string, body any, token string, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求体: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("构造请求 %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", headerAuthScheme+token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// 连接被拒/DNS 失败/超时都走这里：只返回 error，由主循环记日志下轮再试。
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("读取响应 %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return parseAPIError(resp.StatusCode, raw)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析响应 %s %s: %w", method, path, err)
		}
	}
	return nil
}

// parseAPIError 解契约错误体 {"detail":{"code":..,"code_name":..,"message":..}}，
// 也容忍 {"detail":"人话"} 这种 FastAPI 默认形态，日志里永远有可读原因。
func parseAPIError(status int, raw []byte) *APIError {
	e := &APIError{Status: status, Body: strings.TrimSpace(string(raw))}
	var wrapper struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper.Detail) == 0 {
		return e
	}
	var obj struct {
		Code     int    `json:"code"`
		CodeName string `json:"code_name"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(wrapper.Detail, &obj); err == nil && obj.Code != 0 {
		e.Code, e.Name, e.Message = obj.Code, obj.CodeName, obj.Message
		return e
	}
	var msg string
	if err := json.Unmarshal(wrapper.Detail, &msg); err == nil && msg != "" {
		e.Message = msg
	}
	return e
}

// isoNow 统一 UTC 秒级 ISO8601，与服务端 store.now_iso 同格式，字符串可直接比较排序。
func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }
