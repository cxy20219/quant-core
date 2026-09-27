package factor

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// Server 提供因子注册与跟踪接口。
type Server struct {
	lake     *lake.Lake
	registry *Registry
	logf     func(string, ...any)
}

// NewServer 创建因子服务;注册表位于 <lake>/meta/factors.json。
func NewServer(l *lake.Lake, logf func(string, ...any)) (*Server, error) {
	path := filepath.Join(l.Root, "meta", "factors.json")
	reg, err := LoadRegistry(path)
	if err != nil {
		return nil, err
	}
	return &Server{lake: l, registry: reg, logf: logf}, nil
}

// Handler 返回因子路由。
//
//	GET    /api/factors                     列表(含覆盖率摘要)
//	POST   /api/factors                     注册/更新(JSON body)
//	DELETE /api/factors/{id}                删除
//	GET    /api/factors/catalog             内置因子类型目录
//	GET    /api/factors/{id}/tracking?start&end&horizon&layers
//	GET    /api/factors/{id}/coverage?start&end
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/factors", s.handleCollection)
	mux.HandleFunc("/api/factors/", s.handleItem)
	return mux
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"factors": s.registry.List(),
			"catalog": Catalog(),
			"path":    s.registry.path,
		})
	case http.MethodPost:
		var def Definition
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := dec.Decode(&def); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
			return
		}
		if err := s.registry.Put(&def); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"factor": def})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/factors/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	if parts[0] == "catalog" {
		writeJSON(w, http.StatusOK, map[string]any{"catalog": Catalog()})
		return
	}
	id := parts[0]
	def, ok := s.registry.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "因子 " + id + " 不存在"})
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if err := s.registry.Delete(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	case len(parts) == 2 && parts[1] == "tracking":
		s.handleTracking(w, r, def)
	case len(parts) == 2 && parts[1] == "coverage":
		s.handleCoverage(w, r, def)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleTracking(w http.ResponseWriter, r *http.Request, def *Definition) {
	start, end := dateRange(r)
	horizon := atoiDefault(r.URL.Query().Get("horizon"), 5)
	layers := atoiDefault(r.URL.Query().Get("layers"), 5)
	started := time.Now()
	result, err := Track(s.lake, def, TrackOptions{
		StartDate: start, EndDate: end, Horizon: horizon, Layers: layers,
	})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if s.logf != nil {
		s.logf("factor tracking %s: %s~%s horizon=%d layers=%d samples=%d in %s",
			def.ID, start, end, horizon, layers, len(result.Dates), time.Since(started).Round(time.Millisecond))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request, def *Definition) {
	start, end := dateRange(r)
	sd, err := schema.ParseDate(start)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ed, err := schema.ParseDate(end)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	rows, err := Coverage(s.lake, def, sd, ed)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"factor": def.ID, "coverage": rows})
}

// dateRange 解析 start/end(缺省:近一年)。
func dateRange(r *http.Request) (string, string) {
	q := r.URL.Query()
	start, end := q.Get("start"), q.Get("end")
	now := time.Now()
	if end == "" {
		end = now.Format("20060102")
	}
	if start == "" {
		start = now.AddDate(-1, 0, 0).Format("20060102")
	}
	return start, end
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		if os.IsExist(err) {
			return
		}
	}
}
