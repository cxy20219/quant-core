// Package tsapi 实现 tushare 兼容的 HTTP 数据服务。
//
// 协议(与 tushare pro http api 一致):
//
//	POST /
//	{"api_name": "daily", "token": "...", "params": {...}, "fields": "ts_code,trade_date,close"}
//
// 响应:
//
//	{"code": 0, "msg": null, "data": {"fields": [...], "items": [[...]], "has_more": false}}
package tsapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// API 描述一个对外接口。
type API struct {
	Name    string
	Dataset string
	// BuildFilter 把请求参数转换为扫描过滤条件。
	BuildFilter func(ds *schema.Dataset, params Params) (*query.Filter, error)
	// SelectFields 是默认输出字段(nil 表示全部字段)。
	SelectFields []string
	// Transforms 是输出前的单位换算。
	Transforms []Transform
	// FixedPartitions 是固定的分区限定(如 adj_factor 只取 hfq)。
	FixedPartitions map[string]string
	// PartitionParams 把请求参数映射为分区限定(参数名 -> 分区键)。
	PartitionParams map[string]string
	// DefaultLimit 是未指定 limit 时的行数上限,MaxLimit 是单次请求上限。
	DefaultLimit int
	MaxLimit     int
	// Params 是该接口支持的请求参数(仅用于文档生成)。
	Params []APIParam
}

// Transform 定义字段级单位换算:输出值 = 原值 × Scale。
type Transform struct {
	Field string
	Scale float64
}

// Params 是请求参数(JSON 反序列化后的原始值)。
type Params map[string]any

// Str 读取字符串参数,数字会被格式化。
func (p Params) Str(key string) string {
	v, ok := p[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// Int 读取整数参数,失败返回默认值。
func (p Params) Int(key string, def int) int {
	s := p.Str(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Server 是 tushare 兼容数据服务。
type Server struct {
	Lake   *lake.Lake
	APIs   map[string]*API
	Tokens []string // 非空时校验 token
	Logf   func(format string, args ...any)
	// Scanner 为 nil 时按需创建;服务进程应复用带缓存的扫描器。
	Scanner *query.Scanner
}

// NewServer 创建服务并注册默认接口。
func NewServer(l *lake.Lake) *Server {
	return &Server{
		Lake:    l,
		APIs:    DefaultAPIs(),
		Scanner: query.NewCachedScanner(l, 4096),
	}
}

// NewUncachedServer 创建不带元数据缓存的服务(测试用)。
func NewUncachedServer(l *lake.Lake) *Server {
	return &Server{Lake: l, APIs: DefaultAPIs(), Scanner: query.NewScanner(l)}
}

func (s *Server) scanner() *query.Scanner {
	if s.Scanner != nil {
		return s.Scanner
	}
	return query.NewScanner(s.Lake)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler 返回 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	return mux
}

// HealthHandler 返回健康检查处理器。
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		datasets := map[string]any{}
		for _, name := range s.Lake.Registry.Names() {
			ds, err := s.Lake.Registry.Get(name)
			if err != nil {
				continue
			}
			files, err := s.Lake.WalkFiles(ds, nil)
			if err != nil {
				datasets[name] = map[string]any{"error": err.Error()}
				continue
			}
			datasets[name] = map[string]any{"partitions": len(files)}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"lake":     s.Lake.Root,
			"datasets": datasets,
			"apis":     len(s.APIs),
		})
	})
	mux.HandleFunc("/", s.handle)
	return mux
}

type apiRequest struct {
	APIName string         `json:"api_name"`
	Token   string         `json:"token"`
	Params  map[string]any `json:"params"`
	Fields  string         `json:"fields"`
	rawGet  map[string]any // GET 形式的平铺参数
}

type apiResponse struct {
	Code int      `json:"code"`
	Msg  *string  `json:"msg"`
	Data *apiData `json:"data"`
}

type apiData struct {
	Fields  []string `json:"fields"`
	Items   [][]any  `json:"items"`
	HasMore bool     `json:"has_more"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	req, err := parseRequest(r)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if len(s.Tokens) > 0 && !contains(s.Tokens, req.Token) {
		s.writeError(w, errors.New("invalid token"))
		return
	}
	api, ok := s.APIs[req.APIName]
	if !ok {
		s.writeError(w, fmt.Errorf("unknown api_name %q", req.APIName))
		return
	}
	data, err := s.execute(api, req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeJSON(w, apiResponse{Code: 0, Data: data})
}

func parseRequest(r *http.Request) (*apiRequest, error) {
	req := &apiRequest{}
	if r.Method == http.MethodPost {
		body := json.NewDecoder(r.Body)
		body.UseNumber()
		var raw struct {
			APIName string         `json:"api_name"`
			Token   string         `json:"token"`
			Params  map[string]any `json:"params"`
			Fields  string         `json:"fields"`
		}
		if err := body.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid request body: %w", err)
		}
		req.APIName = raw.APIName
		req.Token = raw.Token
		req.Params = raw.Params
		req.Fields = raw.Fields
	} else {
		q := r.URL.Query()
		req.APIName = q.Get("api_name")
		req.Token = q.Get("token")
		req.Fields = q.Get("fields")
		flat := map[string]any{}
		for key, values := range q {
			if len(values) == 0 {
				continue
			}
			flat[key] = values[0]
		}
		req.Params = flat
	}
	if req.APIName == "" {
		return nil, errors.New("api_name is required")
	}
	if req.Params == nil {
		req.Params = map[string]any{}
	}
	return req, nil
}

func (s *Server) execute(api *API, req *apiRequest) (*apiData, error) {
	ds, err := s.Lake.Registry.Get(api.Dataset)
	if err != nil {
		return nil, err
	}
	params := Params(req.Params)
	filter, err := api.BuildFilter(ds, params)
	if err != nil {
		return nil, err
	}

	// 输出字段:接口默认 ∩ 请求 fields
	outFields, err := resolveFields(ds, api, req.Fields)
	if err != nil {
		return nil, err
	}

	// 分页
	limit := params.Int("limit", api.DefaultLimit)
	if limit <= 0 {
		limit = api.DefaultLimit
	}
	if api.MaxLimit > 0 && limit > api.MaxLimit {
		limit = api.MaxLimit
	}
	offset := params.Int("offset", 0)
	if offset < 0 {
		offset = 0
	}

	cur, err := s.scanner().Open(query.Request{
		Dataset:    api.Dataset,
		Filter:     filter,
		Columns:    outFields,
		Limit:      limit + 1, // 多读一行判断 has_more
		Offset:     offset,
		Partitions: api.resolvePartitions(params),
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close()

	items := make([][]any, 0, limit)
	hasMore := false
	for cur.Next() {
		if len(items) >= limit {
			hasMore = true
			break
		}
		row := cur.Row()
		item := make([]any, len(outFields))
		for i, name := range outFields {
			v := api.applyTransform(name, row[i])
			item[i] = outputValue(v)
		}
		items = append(items, item)
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	count := cur.Stats().RowsSeen
	s.logf("api=%s dataset=%s offset=%d limit=%d rows_seen=%d items=%d files=%d rg_read=%d rg_skip=%d",
		api.Name, api.Dataset, offset, limit, count, len(items),
		cur.Stats().FilesOpened, cur.Stats().RowGroupsRead, cur.Stats().RowGroupsSkipped)

	return &apiData{Fields: outFields, Items: items, HasMore: hasMore}, nil
}

func (api *API) applyTransform(name string, v schema.Value) schema.Value {
	for _, t := range api.Transforms {
		if t.Field == name && v.Kind == schema.KindFloat {
			return schema.Float(v.F * t.Scale)
		}
	}
	return v
}

// resolvePartitions 计算请求的分区限定:固定值兜底,参数可覆盖。
func (api *API) resolvePartitions(params Params) map[string][]string {
	if len(api.FixedPartitions) == 0 && len(api.PartitionParams) == 0 {
		return nil
	}
	out := map[string][]string{}
	for key, value := range api.FixedPartitions {
		out[key] = []string{value}
	}
	for param, key := range api.PartitionParams {
		if v := params.Str(param); v != "" {
			out[key] = []string{v}
		}
	}
	return out
}

// resolveFields 计算输出字段:接口默认字段与请求 fields 的交集,保持请求顺序。
func resolveFields(ds *schema.Dataset, api *API, requested string) ([]string, error) {
	defaults := api.SelectFields
	if len(defaults) == 0 {
		defaults = ds.FieldNames()
	}
	if strings.TrimSpace(requested) == "" {
		return append([]string(nil), defaults...), nil
	}
	allowed := make(map[string]bool, len(defaults))
	for _, name := range defaults {
		allowed[name] = true
	}
	var out []string
	for _, name := range strings.Split(requested, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !allowed[name] {
			return nil, fmt.Errorf("field %q is not available for api %s", name, api.Name)
		}
		if _, ok := ds.FieldIndex(name); !ok {
			return nil, fmt.Errorf("field %q does not exist", name)
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return append([]string(nil), defaults...), nil
	}
	return out, nil
}

// outputValue 把逻辑值转换为 tushare JSON 值。
func outputValue(v schema.Value) any {
	if v.IsNull() {
		return nil
	}
	switch v.Kind {
	case schema.KindDate:
		return schema.FormatDate(v.I)
	case schema.KindTimestamp:
		return schema.FormatTimestamp(v.I)
	case schema.KindFloat:
		return v.F
	case schema.KindString:
		return v.S
	case schema.KindInt:
		return v.I
	case schema.KindBool:
		return v.B
	default:
		return nil
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, resp apiResponse) {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(resp)
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	msg := err.Error()
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(apiResponse{Code: -1, Msg: &msg})
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// sortedFieldNames 用于文档/调试输出。
func sortedFieldNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

var _ = sortedFieldNames
