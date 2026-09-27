// Package btworker 管理 Python 策略执行子进程,并通过 stdio JSON-RPC 与回测引擎通信。
//
// 设计:
//   - Go 引擎持有全部行情/账户/撮合语义(唯一权威);
//   - Python 子进程只运行策略代码,通过 RPC 查询行情、下单、读账户;
//   - 协议为逐行 JSON,同步请求-响应,便于单步调试与失败定位。
package btworker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"quant-core/internal/factor"
)

// Config 是 worker 配置。
type Config struct {
	// PythonBinary 是 Python 解释器路径(默认 python3 / python)。
	PythonBinary string
	// WorkerScript 是 worker.py 路径。
	WorkerScript string
	// StrategySource 是策略代码。
	StrategySource string
}

// Worker 是 Python 策略执行器。
type Worker struct {
	cfg    Config
	host   *hostView
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	// phase 是当前回调阶段(handle_data/run_daily/...);order() 在 handle_data 内
	// 需要先撮合可成交挂单(与 quantbt 的 _match_open_orders_before_order 一致)。
	phase string
	// stockBasic 是 stock_basic 快照缓存(get_Ashares / get_stock_name 用)
	stockBasic map[string]stockInfo
	// factors 是因子注册表缓存(get_factor 用)
	factors *factor.Registry
}

// New 创建 worker(不启动进程)。
func New(cfg Config) *Worker {
	if cfg.PythonBinary == "" {
		cfg.PythonBinary = defaultPython()
	}
	return &Worker{cfg: cfg}
}

func defaultPython() string {
	if runtime.GOOS == "windows" {
		return "python"
	}
	return "python3"
}

// DefaultWorkerScript 返回仓库内 worker.py 的默认路径。
func DefaultWorkerScript() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "python", "runner", "worker.py")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "python", "runner", "worker.py")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join("python", "runner", "worker.py")
}

// message 是协议消息(双向复用)。
type message struct {
	Type        string                    `json:"type"`
	ID          int                       `json:"id,omitempty"`
	Method      string                    `json:"method,omitempty"`
	Params      json.RawMessage           `json:"params,omitempty"`
	Result      json.RawMessage           `json:"result,omitempty"`
	Error       string                    `json:"error,omitempty"`
	Message     string                    `json:"message,omitempty"`
	Traceback   string                    `json:"traceback,omitempty"`
	Level       string                    `json:"level,omitempty"`
	Name        string                    `json:"name,omitempty"`
	Day         string                    `json:"day,omitempty"`
	Time        string                    `json:"time,omitempty"`
	PreviousDay string                    `json:"previous_day,omitempty"`
	StrategySrc string                    `json:"strategy_source,omitempty"`
	Meta        map[string]any            `json:"meta,omitempty"`
	Bars        map[string]map[string]any `json:"bars,omitempty"`
	// Codes/Series 是列式行情载荷(字段数组只出现一次,策略侧按需构建 bar 对象)
	Codes     []string       `json:"codes,omitempty"`
	Series    map[string]any `json:"series,omitempty"`
	Portfolio map[string]any `json:"portfolio,omitempty"`
}

func (w *Worker) send(msg *message) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := w.stdin.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("写入策略进程失败: %w", err)
	}
	return nil
}

func (w *Worker) read() (*message, error) {
	line, err := w.stdout.ReadBytes('\n')
	if err != nil {
		if len(line) == 0 {
			return nil, err
		}
	}
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("解析策略进程输出失败: %w(原文: %s)", err, string(line))
	}
	return &msg, nil
}

func (w *Worker) start() error {
	script := w.cfg.WorkerScript
	if script == "" {
		script = DefaultWorkerScript()
	}
	cmd := exec.Command(w.cfg.PythonBinary, script)
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Python 策略进程失败(%s %s): %w", w.cfg.PythonBinary, script, err)
	}
	w.cmd = cmd
	w.stdin = stdin
	w.stdout = bufio.NewReaderSize(stdout, 1<<20)
	return nil
}
