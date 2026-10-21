package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 工具集测试：文件工具必须走路径硬边界，cmd 必须 argv 形式（注入面为零）。

func invoke(t *testing.T, tool Tool, args string) (any, error) {
	t.Helper()
	return tool.Invoke(context.Background(), json.RawMessage(args))
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestFileWriteThenRead(t *testing.T) {
	g := newTestGuard(t)
	w := &fileWriteTool{guard: g}
	r := &fileReadTool{guard: g}

	if _, err := invoke(t, w, `{"path":"d/hello.txt","content_b64":"`+b64("w3 你好\n")+`"}`); err != nil {
		t.Fatalf("写: %v", err)
	}
	got, err := invoke(t, r, `{"path":"d/hello.txt"}`)
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	res, ok := got.(fileResult)
	if !ok {
		t.Fatalf("读结果类型不对: %T", got)
	}
	if res.Content != "w3 你好\n" {
		t.Fatalf("内容不一致: %q", res.Content)
	}
	if res.SizeBytes != len("w3 你好\n") {
		t.Fatalf("大小不对: %d", res.SizeBytes)
	}
	// 落盘权限必须是 0640：同组只读，其他人无权限。
	st, err := os.Stat(filepath.Join(g.root, "d/hello.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o640 {
		t.Fatalf("权限应为 0640，实际 %v", st.Mode().Perm())
	}
}

func TestFileWriteRejectsEscape(t *testing.T) {
	g := newTestGuard(t)
	w := &fileWriteTool{guard: g}
	for _, p := range []string{"../escape.txt", "/etc/tap-escape.txt"} {
		args, _ := json.Marshal(map[string]string{"path": p, "content_b64": b64("x")})
		if _, err := w.Invoke(context.Background(), args); err == nil {
			t.Fatalf("越界写未被拦截: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(g.root), "escape.txt")); err == nil {
		t.Fatal("越界文件竟然真的落盘了")
	}
}

func TestFileWriteBadBase64(t *testing.T) {
	g := newTestGuard(t)
	w := &fileWriteTool{guard: g}
	if _, err := invoke(t, w, `{"path":"a.txt","content_b64":"!!!not-b64!!!"}`); err == nil {
		t.Fatal("坏 base64 未被拦截")
	}
}

func TestDecodeBase64Tolerant(t *testing.T) {
	want := "multi\nline\n"
	for _, in := range []string{
		base64.StdEncoding.EncodeToString([]byte(want)),
		base64.RawStdEncoding.EncodeToString([]byte(want)),
		base64.URLEncoding.EncodeToString([]byte(want)),
		// 脚本 base64 命令会按 76 列折行，换行必须能吃下去。
		strings.ReplaceAll(base64.StdEncoding.EncodeToString([]byte(want)), "", "") + "\n",
	} {
		got, err := decodeB64(in)
		if err != nil {
			t.Fatalf("解码 %q: %v", in, err)
		}
		if string(got) != want {
			t.Fatalf("解码结果不对: %q", got)
		}
	}
}

func TestCmdArgvNotShell(t *testing.T) {
	// 参数里的 shell 元字符必须原样传给子进程，不能被解释执行。
	marker := filepath.Join(t.TempDir(), "pwned")
	payload := "ok; touch " + marker + " && echo pwned"
	got, err := invoke(t, &cmdTool{}, `{"command":"echo","args":["`+payload+`"]}`)
	if err != nil {
		t.Fatalf("echo 失败: %v", err)
	}
	res := got.(cmdResult)
	if strings.TrimSpace(res.Stdout) != payload {
		t.Fatalf("stdout 被解释执行了: %q", res.Stdout)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("注入生效了：子进程真的执行了 touch")
	}
}

func TestCmdRejectsShellEntrypoint(t *testing.T) {
	for _, sh := range []string{"sh", "/bin/bash", "zsh", "powershell.exe"} {
		args, _ := json.Marshal(cmdArgs{Command: sh, Args: []string{"-c", "echo hi"}})
		if _, err := (&cmdTool{}).Invoke(context.Background(), args); err == nil {
			t.Fatalf("shell 入口未被拒绝: %s", sh)
		}
	}
}

func TestCmdExitCodeReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows 下没有稳定的 ls 失败用例")
	}
	args, _ := json.Marshal(cmdArgs{Command: "ls", Args: []string{"/definitely/not/here"}})
	_, err := (&cmdTool{}).Invoke(context.Background(), args)
	if err == nil {
		t.Fatal("非零退出码应返回 error")
	}
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("error 应携带退出码: %T", err)
	}
	if toolErr.ExitCode == 0 {
		t.Fatal("退出码不应为 0")
	}
}

func TestCmdEmptyCommand(t *testing.T) {
	if _, err := invoke(t, &cmdTool{}, `{"command":"  ","args":[]}`); err == nil {
		t.Fatal("空命令未被拦截")
	}
}

func TestExecuteStepUnknownTool(t *testing.T) {
	step := executeStep(context.Background(), 1, stepSpec{Tool: "rm_rf", Args: json.RawMessage("{}")})
	if step.OK {
		t.Fatal("未注册工具不该报成功")
	}
	if !strings.Contains(step.Error, "未注册") {
		t.Fatalf("错误原因不含未注册: %q", step.Error)
	}
	if step.StartedAt == "" || step.FinishedAt == "" {
		t.Fatal("步骤必须带时间戳（契约必填）")
	}
}

func TestParseStepSpecsShapes(t *testing.T) {
	cases := map[string]int{
		`[{"tool":"file_read","args":{"path":"a"}}]`:             1,
		`{"steps":[{"tool":"file_read"},{"tool":"file_write"}]}`: 2,
		`{"tool":"cmd","args":{"command":"echo"}}`:               1,
		`[{"args":{"x":1}}]`:                                     0, // 缺 tool 丢掉
		`只是一个普通摘要，不是 JSON`:                                       0,
		`{"hello":"world"}`:                                      0,
	}
	for in, want := range cases {
		if got := len(parseStepSpecs(json.RawMessage(in))); got != want {
			t.Fatalf("%s 解析出 %d 步，期望 %d", in, got, want)
		}
	}
}

func TestPlannedStepsPrefersStepsField(t *testing.T) {
	summary := "[{\"tool\":\"from_summary\"}]"
	task := InboxTask{
		Title:   "普通标题",
		Summary: &summary,
		Steps:   json.RawMessage(`[{"tool":"from_steps"}]`),
	}
	specs, err := plannedSteps(task)
	if err != nil {
		t.Fatalf("解指令: %v", err)
	}
	if len(specs) != 1 || specs[0].Tool != "from_steps" {
		t.Fatalf("应优先用 steps 字段: %+v", specs)
	}
}

func TestPlannedStepsFallsBackToSummary(t *testing.T) {
	summary := `{"steps":[{"tool":"file_read","args":{"path":"a.txt"}}]}`
	specs, err := plannedSteps(InboxTask{Title: "普通标题", Summary: &summary})
	if err != nil {
		t.Fatalf("解指令: %v", err)
	}
	if len(specs) != 1 || string(specs[0].Args) != `{"path":"a.txt"}` {
		t.Fatalf("args 透传不对: %s", specs[0].Args)
	}
}

func TestPlannedStepsNoInstruction(t *testing.T) {
	if _, err := plannedSteps(InboxTask{Title: "整理周报"}); err == nil {
		t.Fatal("无指令任务应返回 error（由上报层记失败步骤）")
	}
}

// ---------------------------------------------------------------------------
// HTTP 客户端
// ---------------------------------------------------------------------------

func TestClientAuthAndHeartbeat(t *testing.T) {
	var authCalls atomic.Int32
	var gotAuth atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		authCalls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["api_key"] != "k" || body["device_fingerprint"] != "fp" {
			t.Errorf("认证体不对: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":       "tok-1",
			"expires_at":  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"user_id":     "u1",
			"terminal_id": "term1",
		})
	})
	mux.HandleFunc("/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["version"] != agentVersion {
			t.Errorf("心跳版本不对: %v", body["version"])
		}
		if body["queue_len"] != float64(3) {
			t.Errorf("queue_len 不对: %v", body["queue_len"])
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "k", "fp")
	if err := c.Auth(context.Background()); err != nil {
		t.Fatalf("认证: %v", err)
	}
	if c.TerminalID() != "term1" {
		t.Fatalf("terminal_id 未保存: %s", c.TerminalID())
	}
	if err := c.Heartbeat(context.Background(), 3); err != nil {
		t.Fatalf("心跳: %v", err)
	}
	if got := gotAuth.Load(); got != "Bearer tok-1" {
		t.Fatalf("心跳未带 token: %v", got)
	}
	if authCalls.Load() != 1 {
		t.Fatalf("认证次数应为 1，实际 %d", authCalls.Load())
	}
}

func TestClientReauthOnceOn40101(t *testing.T) {
	var authCalls, beatCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		n := authCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "tok-" + string(rune('0'+n)),
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		beatCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":{"code":40101,"code_name":"AUTH_FAILED","message":"api_key 无效"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "k", "fp")
	if err := c.Auth(context.Background()); err != nil {
		t.Fatalf("首次认证: %v", err)
	}
	if err := c.Heartbeat(context.Background(), 0); err != nil {
		t.Fatalf("40101 后应重认证成功: %v", err)
	}
	if authCalls.Load() != 2 {
		t.Fatalf("应恰好重认证 1 次，实际认证 %d 次", authCalls.Load())
	}
	if beatCalls.Load() != 2 {
		t.Fatalf("应重试 1 次，实际请求 %d 次", beatCalls.Load())
	}
}

func TestClientNon401ErrorDoesNotReauth(t *testing.T) {
	var authCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		authCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "tok",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/v1/inbox/pull", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"库存炸了"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "k", "fp")
	_, err := c.PullInbox(context.Background())
	if err == nil {
		t.Fatal("500 应返回 error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 {
		t.Fatalf("应返回 APIError(500): %v", err)
	}
	if authCalls.Load() != 1 {
		t.Fatalf("非 401 不该重认证，认证了 %d 次", authCalls.Load())
	}
}

func TestClientNetworkErrorNoPanic(t *testing.T) {
	// 指向一个已经关掉的端口：必须返回 error，不许 panic。
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	c := NewHTTPClient(url, "k", "fp")
	if _, err := c.PullInbox(context.Background()); err == nil {
		t.Fatal("网络错误应返回 error")
	}
}

func TestClientPullInboxAndReportStepsPayload(t *testing.T) {
	var reportPath string
	var reportBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "tok",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/v1/inbox/pull", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tasks":[{"id":"task_1","title":"t","summary":"s","status":"running"}],"count":1}`))
	})
	mux.HandleFunc("/v1/tasks/task_1/steps", func(w http.ResponseWriter, r *http.Request) {
		reportPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&reportBody)
		_, _ = w.Write([]byte(`{"count":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, "k", "fp")
	tasks, err := c.PullInbox(context.Background())
	if err != nil {
		t.Fatalf("拉 inbox: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "task_1" || tasks[0].Summary == nil || *tasks[0].Summary != "s" {
		t.Fatalf("任务解析不对: %+v", tasks)
	}

	code := 2
	steps := []reportStep{{Seq: 1, Tool: "cmd", OK: false, StartedAt: isoNow(), Error: "退出码 2", ExitCode: &code}}
	if err := c.ReportSteps(context.Background(), "task_1", steps, "failed"); err != nil {
		t.Fatalf("上报: %v", err)
	}
	if reportPath != "/v1/tasks/task_1/steps" {
		t.Fatalf("上报路径不对: %s", reportPath)
	}
	if reportBody["request_id"] != "exec-task_1" {
		t.Fatalf("幂等键不对: %v", reportBody["request_id"])
	}
	if reportBody["status"] != "failed" {
		t.Fatalf("状态不对: %v", reportBody["status"])
	}
	first := reportBody["steps"].([]any)[0].(map[string]any)
	// 契约必填三件套必须在报文里出现过。
	for _, key := range []string{"tool", "ok", "started_at"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("步骤缺契约必填字段 %s: %v", key, first)
		}
	}
}
