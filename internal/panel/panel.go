// Package panel 提供内置管理面板:数据同步状态、回测结果,以及后续的因子管理入口。
//
// 设计:与服务同进程、自包含 HTML(内联样式与脚本,无 CDN/构建步骤),
// 数据来自湖的 manifest、数据集注册表与源注册表;回测数据直接复用 /api/backtests。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/source"
)

// Config 是面板依赖。
type Config struct {
	Lake        *lake.Lake
	SourcesPath string // sources.yaml(可选;缺失则隐藏源状态)
	Logf        func(format string, args ...any)
}

// Server 是面板处理器。
type Server struct {
	cfg     Config
	started time.Time
	mu      sync.Mutex
	// disk 缓存:数据集 → (字节数, 文件数, 分区数, 缓存时间)
	disk map[string]diskStat
}

type diskStat struct {
	Bytes      int64
	Files      int
	Partitions int
	At         time.Time
}

// NewServer 创建面板。
func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, started: time.Now(), disk: map[string]diskStat{}}
}

// Handler 返回面板路由(/panel 与 /panel/api/*)。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/panel", s.handlePage)
	mux.HandleFunc("/panel/", s.handlePage)
	mux.HandleFunc("/panel/api/overview", s.handleOverview)
	mux.HandleFunc("/panel/api/sync", s.handleSync)
	mux.HandleFunc("/panel/api/sources", s.handleSources)
	return mux
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, map[string]any{
		"Started": s.started.Format("2006-01-02 15:04:05"),
	}); err != nil {
		s.logf("panel: %v", err)
	}
}

// ── JSON 接口 ───────────────────────────────────────────────────────────

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	datasets := s.datasetNames()
	var totalRows, totalFiles, totalBytes int64
	lastSync := time.Time{}
	for _, name := range datasets {
		summary, err := s.summarize(name)
		if err == nil {
			totalRows += summary.Rows
			totalFiles += int64(summary.Files)
			if summary.LastTime.After(lastSync) {
				lastSync = summary.LastTime
			}
		}
		stat := s.diskStat(name)
		totalBytes += stat.Bytes
	}
	writeJSON(w, map[string]any{
		"service": map[string]any{
			"started":      s.started.Format(time.RFC3339),
			"uptime":       time.Since(s.started).Round(time.Second).String(),
			"lake":         s.cfg.Lake.Root,
			"datasets":     len(datasets),
			"sources":      s.sourcesPath(),
			"generated_at": time.Now().Format(time.RFC3339),
		},
		"lake": map[string]any{
			"total_rows":  totalRows,
			"total_files": totalFiles,
			"total_bytes": totalBytes,
			"last_sync":   formatTime(lastSync),
		},
	})
}

// syncRow 是数据同步表的一行。
type syncRow struct {
	Dataset     string     `json:"dataset"`
	Description string     `json:"description"`
	Partitions  string     `json:"partitions"`
	Fields      int        `json:"fields"`
	Rows        int64      `json:"rows"`
	Batches     int        `json:"batches"`
	Files       int        `json:"files"`
	DiskBytes   int64      `json:"disk_bytes"`
	DiskFiles   int        `json:"disk_files"`
	DiskParts   int        `json:"disk_partitions"`
	LastTime    string     `json:"last_time"`
	LastOp      string     `json:"last_op"`
	DaysAgo     int        `json:"days_ago"`
	Recent      []batchRow `json:"recent"`
	Missing     bool       `json:"missing"` // 注册了但湖里没有数据文件
}

type batchRow struct {
	ID        string `json:"id"`
	Operation string `json:"operation"`
	Source    string `json:"source"`
	Rows      int64  `json:"rows"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	rows := []syncRow{}
	now := time.Now()
	for _, name := range s.datasetNames() {
		ds, err := s.cfg.Lake.Registry.Get(name)
		if err != nil {
			continue
		}
		row := syncRow{
			Dataset:     name,
			Description: ds.Description,
			Partitions:  strings.Join(ds.Partitions, "/"),
			Fields:      len(ds.Fields),
		}
		if summary, err := s.summarize(name); err == nil {
			row.Rows, row.Batches, row.Files = summary.Rows, summary.Batches, summary.Files
			if !summary.LastTime.IsZero() {
				row.LastTime = summary.LastTime.Local().Format("2006-01-02 15:04:05")
				row.DaysAgo = int(now.Sub(summary.LastTime).Hours() / 24)
			}
		}
		if batches, err := s.batches(name, 8); err == nil {
			row.Recent = batches
			if len(batches) > 0 {
				row.LastOp = batches[len(batches)-1].Operation
			}
		}
		stat := s.diskStat(name)
		row.DiskBytes, row.DiskFiles, row.DiskParts = stat.Bytes, stat.Files, stat.Partitions
		row.Missing = stat.Files == 0
		rows = append(rows, row)
	}
	writeJSON(w, map[string]any{"datasets": rows, "generated_at": time.Now().Format(time.RFC3339)})
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	path := s.sourcesPath()
	if path == "" {
		writeJSON(w, map[string]any{"sources": []any{}, "bindings": map[string][]string{}, "note": "未配置 sources.yaml"})
		return
	}
	reg, err := source.LoadRegistry(path)
	if err != nil {
		writeJSON(w, map[string]any{"sources": []any{}, "bindings": map[string][]string{}, "note": "加载失败: " + err.Error()})
		return
	}
	defer func() { _ = reg.Close() }()
	infos := reg.Infos()
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, map[string]any{
			"name": info.Name, "kind": info.Kind, "description": info.Description,
			"disabled": info.Disabled,
		})
	}
	writeJSON(w, map[string]any{
		"sources":  out,
		"bindings": reg.Bindings(),
		"path":     path,
		"note":     "此处仅显示配置状态;连通性检查请在宿主机执行 `quantd source check`(容器内无密钥)",
	})
}

// ── 内部 ────────────────────────────────────────────────────────────────

func (s *Server) datasetNames() []string {
	names := make([]string, 0, len(s.cfg.Lake.Registry.Datasets))
	for name := range s.cfg.Lake.Registry.Datasets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) summarize(dataset string) (lake.BatchSummary, error) {
	logger := lake.NewBatchLogger(s.cfg.Lake.Root)
	return logger.Summarize(dataset)
}

func (s *Server) batches(dataset string, limit int) ([]batchRow, error) {
	logger := lake.NewBatchLogger(s.cfg.Lake.Root)
	all, err := logger.Read(dataset)
	if err != nil {
		return nil, err
	}
	out := make([]batchRow, 0, limit)
	start := len(all) - limit
	if start < 0 {
		start = 0
	}
	for _, b := range all[start:] {
		out = append(out, batchRow{
			ID: b.ID, Operation: b.Operation, Source: b.Source, Rows: b.Rows,
			CreatedAt: b.CreatedAt.Local().Format("2006-01-02 15:04:05"),
		})
	}
	return out, nil
}

// diskStat 统计数据集目录的字节数/文件数/分区数(60 秒缓存)。
func (s *Server) diskStat(dataset string) diskStat {
	s.mu.Lock()
	if stat, ok := s.disk[dataset]; ok && time.Since(stat.At) < time.Minute {
		s.mu.Unlock()
		return stat
	}
	s.mu.Unlock()

	ds, err := s.cfg.Lake.Registry.Get(dataset)
	if err != nil {
		return diskStat{}
	}
	root := s.cfg.Lake.Dir(ds)
	stat := diskStat{At: time.Now()}
	parts := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			if rel, rerr := filepath.Rel(root, path); rerr == nil && rel != "." && strings.Count(rel, string(filepath.Separator)) == 0 {
				parts[rel] = true
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".parquet") {
			if info, ierr := d.Info(); ierr == nil {
				stat.Bytes += info.Size()
				stat.Files++
			}
		}
		return nil
	})
	stat.Partitions = len(parts)
	s.mu.Lock()
	s.disk[dataset] = stat
	s.mu.Unlock()
	return stat
}

func (s *Server) sourcesPath() string {
	if s.cfg.SourcesPath == "" {
		return ""
	}
	if _, err := os.Stat(s.cfg.SourcesPath); err != nil {
		return ""
	}
	return s.cfg.SourcesPath
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		http.Error(w, fmt.Sprintf("encode: %v", err), http.StatusInternalServerError)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
