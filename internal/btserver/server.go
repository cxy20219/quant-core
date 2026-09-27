// Package btserver 提供回测服务的 HTTP 接口与作业管理。
//
// POST /api/backtests  提交回测(策略代码 + 参数 + 区间)
// GET  /api/backtests  列出历史记录
// GET  /api/backtests/{id}  查询状态与结果
package btserver

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"quant-core/internal/btengine"
	"quant-core/internal/btworker"
	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// Config 是回测服务配置。
type Config struct {
	// MaxConcurrent 是并发回测数上限。
	MaxConcurrent int
	// RecordsDir 是结果持久化目录。
	RecordsDir string
	// PythonBinary / WorkerScript 透传给 worker。
	PythonBinary string
	WorkerScript string
}

// Server 是回测服务。
type Server struct {
	Lake   *lake.Lake
	Config Config

	mu    sync.RWMutex
	jobs  map[string]*Job
	order []string
	sem   chan struct{}
	Logf  func(format string, args ...any)
}

// Job 是一次回测作业。
type Job struct {
	ID           string           `json:"id"`
	Status       string           `json:"status"` // queued / running / done / failed
	StrategyName string           `json:"strategy_name"`
	Params       map[string]any   `json:"params,omitempty"`
	Request      *BacktestRequest `json:"request"`
	CreatedAt    string           `json:"created_at"`
	StartedAt    string           `json:"started_at,omitempty"`
	FinishedAt   string           `json:"finished_at,omitempty"`
	Error        string           `json:"error,omitempty"`
	Result       *btengine.Result `json:"result,omitempty"`
	Summary      *JobSummary      `json:"summary,omitempty"`
}

// JobSummary 是列表页使用的概要。
type JobSummary struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	StrategyName string  `json:"strategy_name"`
	CreatedAt    string  `json:"created_at"`
	StartDate    string  `json:"start_date"`
	EndDate      string  `json:"end_date"`
	TotalReturn  float64 `json:"total_return"`
	MaxDrawdown  float64 `json:"max_drawdown"`
	SharpeRatio  float64 `json:"sharpe_ratio"`
	OrderCount   int     `json:"order_count"`
	TradeCount   int     `json:"trade_count"`
}

// BacktestRequest 是提交回测的请求体。
type BacktestRequest struct {
	StrategyCode  string         `json:"strategy_code"`
	StrategyName  string         `json:"strategy_name"`
	Params        map[string]any `json:"params"`
	StartDate     string         `json:"start_date"`
	EndDate       string         `json:"end_date"`
	CapitalBase   float64        `json:"capital_base"`
	Benchmark     string         `json:"benchmark"`
	Frequency     string         `json:"frequency"`
	MinuteMaxRows int            `json:"minute_max_rows"`
	WarmupDays    int            `json:"warmup_days"`
}

// NewServer 创建回测服务。
func NewServer(l *lake.Lake, cfg Config) *Server {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 2
	}
	return &Server{
		Lake:   l,
		Config: cfg,
		jobs:   map[string]*Job{},
		sem:    make(chan struct{}, cfg.MaxConcurrent),
		Logf:   log.Printf,
	}
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/backtests", s.handleCollection)
	mux.HandleFunc("/api/backtests/", s.handleItem)
	return mux
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.submit(w, r)
	case http.MethodGet:
		s.list(w)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/backtests/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	job, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok {
		if job = s.loadRecord(id); job == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "backtest not found"})
			return
		}
		s.mu.Lock()
		s.jobs[id] = job
		s.order = append(s.order, id)
		s.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req BacktestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if err := validate(&req); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	job := &Job{
		ID:           uuid.NewString(),
		Status:       "queued",
		StrategyName: req.StrategyName,
		Params:       req.Params,
		Request:      &req,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}
	s.mu.Lock()
	s.jobs[job.ID] = job
	s.order = append(s.order, job.ID)
	s.mu.Unlock()
	if err := s.persist(job); err != nil {
		s.logf("backtest %s 记录落盘失败: %v", shortID(job.ID), err)
	}

	go s.run(job)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": job.ID, "status": job.Status})
}

func validate(req *BacktestRequest) error {
	if strings.TrimSpace(req.StrategyCode) == "" {
		return fmt.Errorf("strategy_code 不能为空")
	}
	if len(req.StrategyCode) > 500_000 {
		return fmt.Errorf("strategy_code 过大(>500KB)")
	}
	if req.StartDate == "" || req.EndDate == "" {
		return fmt.Errorf("start_date / end_date 必填(YYYYMMDD 或 YYYY-MM-DD)")
	}
	if _, err := schema.ParseDate(req.StartDate); err != nil {
		return fmt.Errorf("start_date: %w", err)
	}
	if _, err := schema.ParseDate(req.EndDate); err != nil {
		return fmt.Errorf("end_date: %w", err)
	}
	if req.StrategyName == "" {
		req.StrategyName = "未命名策略"
	}
	if req.CapitalBase <= 0 {
		req.CapitalBase = 1_000_000
	}
	if req.WarmupDays <= 0 {
		req.WarmupDays = 365
	}
	return nil
}

// run 执行一次回测作业。
func (s *Server) run(job *Job) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	s.mu.Lock()
	job.Status = "running"
	job.StartedAt = time.Now().Format(time.RFC3339)
	s.mu.Unlock()
	if err := s.persist(job); err != nil {
		s.logf("backtest %s 记录落盘失败: %v", shortID(job.ID), err)
	}
	s.logf("backtest %s 开始: %s", shortID(job.ID), job.StrategyName)

	req := job.Request
	freq := req.Frequency
	if freq == "" {
		freq = "1d"
	}
	warmupDays := req.WarmupDays
	if warmupDays <= 0 && freq == "1m" {
		warmupDays = 30 // 分钟模式默认预热 30 个交易日(供 get_history 分钟序列使用)
	}
	startDays, _ := schema.ParseDate(req.StartDate)
	endDays, _ := schema.ParseDate(req.EndDate)
	cfg := btengine.Config{
		StrategyName: req.StrategyName,
		StartDate:    schema.TimeFromDays(startDays),
		EndDate:      schema.TimeFromDays(endDays),
		Frequency:    freq,

		CapitalBase:   req.CapitalBase,
		Benchmark:     strings.TrimSpace(req.Benchmark),
		WarmupDays:    warmupDays,
		MinuteMaxRows: req.MinuteMaxRows,
		Params:        job.Params,
	}

	worker := btworker.New(btworker.Config{
		PythonBinary:   s.Config.PythonBinary,
		WorkerScript:   s.Config.WorkerScript,
		StrategySource: req.StrategyCode,
	})
	engine := btengine.NewEngine(s.Lake, cfg)
	result, err := engine.Run(worker)
	job.FinishedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		s.logf("backtest %s 失败: %v", shortID(job.ID), err)
	} else {
		job.Status = "done"
		btworker.ApplyDisplayCodes(result)
		job.Result = result
		job.Summary = summarize(job)
		s.logf("backtest %s 完成: 收益 %.2f%%, 委托 %d, 成交 %d, 用时 %.1fs",
			shortID(job.ID), result.Summary.TotalReturn*100, result.Summary.OrderCount,
			result.Summary.TradeCount, result.Summary.ElapsedSecond)
	}
	if err := s.persist(job); err != nil {
		s.logf("backtest %s 结果持久化失败: %v", shortID(job.ID), err)
	}
}

func summarize(job *Job) *JobSummary {
	out := &JobSummary{
		ID:           job.ID,
		Status:       job.Status,
		StrategyName: job.StrategyName,
		CreatedAt:    job.CreatedAt,
	}
	if job.Request != nil {
		out.StartDate = job.Request.StartDate
		out.EndDate = job.Request.EndDate
	}
	if job.Result != nil {
		out.TotalReturn = job.Result.Summary.TotalReturn
		out.OrderCount = job.Result.Summary.OrderCount
		out.TradeCount = job.Result.Summary.TradeCount
		if v, ok := job.Result.Analytics["max_drawdown"].(float64); ok {
			out.MaxDrawdown = v
		}
		if v, ok := job.Result.Analytics["sharpe_ratio"].(float64); ok {
			out.SharpeRatio = v
		}
	}
	return out
}

func (s *Server) list(w http.ResponseWriter) {
	s.mu.RLock()
	ids := append([]string(nil), s.order...)
	s.mu.RUnlock()
	records := make([]*JobSummary, 0, len(ids))
	for _, id := range ids {
		s.mu.RLock()
		job := s.jobs[id]
		s.mu.RUnlock()
		if job == nil {
			continue
		}
		summary := job.Summary
		if summary == nil {
			summary = summarize(job)
		}
		records = append(records, summary)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt > records[j].CreatedAt })
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

func (s *Server) persist(job *Job) error {
	if s.Config.RecordsDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.Config.RecordsDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.Config.RecordsDir, job.ID+".json")
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) loadRecord(id string) *Job {
	if s.Config.RecordsDir == "" || strings.ContainsAny(id, "/\\") {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(s.Config.RecordsDir, id+".json"))
	if err != nil {
		return nil
	}
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil
	}
	return &job
}

// LoadRecords 启动时加载历史记录(列表可见),并把上次进程遗留的
// queued/running 作业标记为失败(否则客户端轮询会看到永远"运行中")。
func (s *Server) LoadRecords() {
	if s.Config.RecordsDir == "" {
		return
	}
	entries, err := os.ReadDir(s.Config.RecordsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		job := s.loadRecord(id)
		if job == nil {
			continue
		}
		if job.Status == "queued" || job.Status == "running" {
			job.Status = "failed"
			job.Error = "服务重启,作业被中断(未完成)"
			job.FinishedAt = time.Now().Format(time.RFC3339)
			if err := s.persist(job); err != nil {
				s.logf("标记中断作业 %s 失败: %v", shortID(id), err)
			}
			s.logf("backtest %s 标记为中断(服务重启)", shortID(id))
		}
		s.mu.Lock()
		s.jobs[id] = job
		s.order = append(s.order, id)
		s.mu.Unlock()
	}
}

// InterruptRunningJobs 停机前把排队中/运行中的作业标记为失败并落盘,
// 让客户端在服务恢复后能看到明确的中断状态(而不是悬空记录)。
func (s *Server) InterruptRunningJobs(reason string) {
	s.mu.Lock()
	jobs := make([]*Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		if job.Status == "queued" || job.Status == "running" {
			job.Status = "failed"
			job.Error = reason
			job.FinishedAt = time.Now().Format(time.RFC3339)
			jobs = append(jobs, job)
		}
	}
	s.mu.Unlock()
	for _, job := range jobs {
		if err := s.persist(job); err != nil {
			s.logf("标记中断作业 %s 失败: %v", shortID(job.ID), err)
		} else {
			s.logf("backtest %s 标记为中断(停机)", shortID(job.ID))
		}
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// shortID 返回日志用的短 ID(兼容非 UUID 的短 ID,避免越界)。
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
