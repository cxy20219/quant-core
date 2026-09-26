package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Exec 是外部命令源插件(kind: exec)。
//
// 任何语言写的获取脚本都可以作为数据源接入,无需重新编译主程序:
//
//	sources:
//	  my-source:
//	    kind: exec
//	    command: ["python", "plugins/akshare_source.py"]
//	    timeout_ms: 60000
//	    env: {AKSHARE_TOKEN: "${AK_TOKEN}"}
//
// 协议(逐次调用,JSON over stdin/stdout):
//
//	stdin : {"action":"call","api":"daily","params":{...},"fields":"ts_code,close"}
//	stdout: {"fields":[...],"items":[[...]],"count":N,"has_more":false}
//	    或  {"error":"..."}
//	探活  : {"action":"check"} → {"ok":true,"note":"..."}
type Exec struct {
	name    string
	command []string
	env     []string
	timeout time.Duration
	logf    func(format string, args ...any)
}

// NewExec 按配置构造外部命令源。
func NewExec(name string, cfg Config) (Source, error) {
	cmd := cfg.Strings("command")
	if len(cmd) == 0 {
		// 兼容 command: "python x.py" 的写法
		if raw := cfg.Str("command", ""); raw != "" {
			cmd = strings.Fields(raw)
		}
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("sources.%s: kind=exec 需要 command(数组形式,如 [\"python\",\"plugin.py\"])", name)
	}
	s := &Exec{
		name:    name,
		command: cmd,
		timeout: time.Duration(cfg.Int("timeout_ms", 60000)) * time.Millisecond,
	}
	for key, value := range cfg.Child("env").Raw() {
		raw := Config{raw: map[string]any{"v": value}, path: cfg.Path() + ".env." + key}
		s.env = append(s.env, key+"="+raw.Str("v", ""))
	}
	return s, nil
}

// SetLogger 注入日志回调。
func (s *Exec) SetLogger(logf func(format string, args ...any)) { s.logf = logf }

func (s *Exec) Name() string { return s.name }
func (s *Exec) Kind() string { return "exec" }
func (s *Exec) Close() error { return nil }

// Call 执行一次插件调用。
func (s *Exec) Call(ctx context.Context, api string, params map[string]any, fields string) (*Result, error) {
	payload := map[string]any{
		"action": "call",
		"api":    api,
		"params": params,
		"fields": fields,
	}
	out, err := s.run(ctx, payload)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Fields  []string       `json:"fields"`
		Items   [][]any        `json:"items"`
		Count   int64          `json:"count"`
		HasMore bool           `json:"has_more"`
		Meta    map[string]any `json:"meta"`
		Error   string         `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("source %s: 插件输出不是合法 JSON: %w(输出: %s)", s.name, err, truncateStr(string(out), 200))
	}
	if resp.Error != "" {
		return nil, &SrcError{Msg: resp.Error}
	}
	if resp.Fields == nil {
		return nil, fmt.Errorf("source %s: 插件输出缺少 fields", s.name)
	}
	return &Result{Fields: resp.Fields, Items: resp.Items, Count: resp.Count, HasMore: resp.HasMore, Meta: resp.Meta}, nil
}

// Check 探活(供 source check 使用)。
func (s *Exec) Check(ctx context.Context) (string, error) {
	out, err := s.run(ctx, map[string]any{"action": "check"})
	if err != nil {
		return "", err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Note  string `json:"note"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("插件输出不是合法 JSON: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("插件返回错误: %s", resp.Error)
	}
	if !resp.OK {
		return "", fmt.Errorf("插件探活失败")
	}
	return resp.Note, nil
}

func (s *Exec) run(ctx context.Context, payload map[string]any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, s.command[0], s.command[1:]...)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Env = append(os.Environ(), s.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("source %s: 插件执行失败: %s", s.name, truncateStr(msg, 300))
	}
	return stdout.Bytes(), nil
}
