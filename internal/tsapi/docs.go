package tsapi

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"quant-core/internal/schema"
)

// DocsHandler 返回接口文档页(自包含 HTML,无外部依赖)。
//
// 内容从单一来源生成:接口表(api_name/参数/限额/单位换算)+ 数据集注册表
// (字段名/类型/单位/说明);回测 REST 与策略 API 为静态说明段。
func (s *Server) DocsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := s.docsData()
		if err := docsTemplate.Execute(w, data); err != nil {
			s.logf("docs: %v", err)
		}
	})
}

// OpenAPIHandler 返回 OpenAPI 3.0 规范(JSON),便于导入 Postman/Swagger。
func (s *Server) OpenAPIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(s.openAPISpec()); err != nil {
			s.logf("openapi: %v", err)
		}
	})
}

// ── 文档数据模型 ─────────────────────────────────────────────────────────

type docsData struct {
	Datasets []datasetDoc
	APIs     []apiDoc
	Base     string
}

type datasetDoc struct {
	Name        string
	Description string
	Partitions  string
	Rows        string
	Fields      []fieldDoc
}

type fieldDoc struct {
	Name string
	Type string
	Unit string
	Desc string
}

type apiDoc struct {
	Name         string
	Dataset      string
	Description  string
	DefaultLimit int
	MaxLimit     int
	Transforms   []string
	Params       []APIParam
	Fields       []fieldDoc
	Example      string
}

func (s *Server) docsData() docsData {
	data := docsData{Base: "/"}
	names := make([]string, 0, len(s.APIs))
	for name := range s.APIs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		api := s.APIs[name]
		ds := s.datasetFor(api.Dataset)
		doc := apiDoc{
			Name:         api.Name,
			Dataset:      api.Dataset,
			DefaultLimit: api.DefaultLimit,
			MaxLimit:     api.MaxLimit,
			Params:       api.Params,
		}
		if ds != nil {
			doc.Description = ds.Description
			doc.Fields = apiFields(api, ds)
		}
		for _, tr := range api.Transforms {
			doc.Transforms = append(doc.Transforms,
				fmt.Sprintf("%s × %g(存储→接口口径)", tr.Field, tr.Scale))
		}
		doc.Example = apiExample(api)
		data.APIs = append(data.APIs, doc)
	}
	if s.Lake != nil && s.Lake.Registry != nil {
		dsNames := make([]string, 0, len(s.Lake.Registry.Datasets))
		for name := range s.Lake.Registry.Datasets {
			dsNames = append(dsNames, name)
		}
		sort.Strings(dsNames)
		for _, name := range dsNames {
			ds := s.Lake.Registry.Datasets[name]
			item := datasetDoc{
				Name:        ds.Name,
				Description: ds.Description,
				Partitions:  strings.Join(ds.Partitions, "/"),
			}
			for _, f := range ds.Fields {
				item.Fields = append(item.Fields, fieldDoc{
					Name: f.Name, Type: string(f.Type), Unit: f.Unit, Desc: f.Desc,
				})
			}
			data.Datasets = append(data.Datasets, item)
		}
	}
	return data
}

func (s *Server) datasetFor(name string) *schema.Dataset {
	if s.Lake == nil || s.Lake.Registry == nil {
		return nil
	}
	ds, err := s.Lake.Registry.Get(name)
	if err != nil {
		return nil
	}
	return ds
}

func apiFields(api *API, ds *schema.Dataset) []fieldDoc {
	names := api.SelectFields
	if len(names) == 0 {
		for _, f := range ds.Fields {
			names = append(names, f.Name)
		}
	}
	out := make([]fieldDoc, 0, len(names))
	for _, name := range names {
		for _, f := range ds.Fields {
			if f.Name == name {
				out = append(out, fieldDoc{Name: f.Name, Type: string(f.Type), Unit: f.Unit, Desc: f.Desc})
				break
			}
		}
	}
	return out
}

func apiExample(api *API) string {
	switch api.Name {
	case "stk_mins":
		return `{"api_name":"stk_mins","params":{"ts_code":"600000.SH","freq":"1min",` +
			`"start_date":"2024-01-02 09:30:00","end_date":"2024-01-02 15:00:00"}}`
	case "stock_basic":
		return `{"api_name":"stock_basic","params":{"exchange":"SSE","list_status":"L"},` +
			`"fields":"ts_code,name,list_date"}`
	case "trade_cal":
		return `{"api_name":"trade_cal","params":{"exchange":"SSE","start_date":"20240101","end_date":"20241231"}}`
	default:
		return fmt.Sprintf(`{"api_name":"%s","params":{"ts_code":"600000.SH",`+
			`"start_date":"20240102","end_date":"20240110"}}`, api.Name)
	}
}

// ── OpenAPI 3.0 ─────────────────────────────────────────────────────────

func (s *Server) openAPISpec() map[string]any {
	apiNames := make([]string, 0, len(s.APIs))
	for name := range s.APIs {
		apiNames = append(apiNames, name)
	}
	sort.Strings(apiNames)
	envelope := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"api_name": map[string]any{"type": "string", "enum": apiNames,
				"description": "数据接口名"},
			"token":  map[string]any{"type": "string"},
			"params": map[string]any{"type": "object", "additionalProperties": true},
			"fields": map[string]any{"type": "string", "description": "逗号分隔的输出字段"},
		},
		"required": []string{"api_name"},
	}
	okResponse := func() map[string]any {
		return map[string]any{
			"description": "成功",
			"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"type": "object", "properties": map[string]any{
					"code": map[string]any{"type": "integer"},
					"msg":  map[string]any{"type": "string", "nullable": true},
					"data": map[string]any{"type": "object", "properties": map[string]any{
						"fields":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"items":    map[string]any{"type": "array", "items": map[string]any{"type": "array"}},
						"has_more": map[string]any{"type": "boolean"},
					}},
				}},
			}},
		}
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "quant-core 数据与回测服务",
			"version":     "1.0.0",
			"description": "tushare 兼容数据接口(POST / + api_name)与回测作业 REST;文档页 /docs",
		},
		"paths": map[string]any{
			"/": map[string]any{
				"post": map[string]any{
					"summary":     "tushare 兼容数据查询",
					"description": "以 api_name 分发到各数据接口;可用接口:" + strings.Join(apiNames, ", "),
					"requestBody": map[string]any{"required": true, "content": map[string]any{
						"application/json": map[string]any{"schema": envelope}}},
					"responses": map[string]any{"200": okResponse()},
				},
			},
			"/api/backtests": map[string]any{
				"post": map[string]any{
					"summary": "提交回测作业",
					"requestBody": map[string]any{"required": true, "content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"strategy_code": map[string]any{"type": "string", "description": "PTrade 兼容 Python 策略源码"},
								"strategy_name": map[string]any{"type": "string"},
								"params":        map[string]any{"type": "object"},
								"start_date":    map[string]any{"type": "string", "example": "20240101"},
								"end_date":      map[string]any{"type": "string", "example": "20241231"},
								"capital_base":  map[string]any{"type": "number", "example": 1000000},
								"benchmark":     map[string]any{"type": "string"},
								"frequency":     map[string]any{"type": "string", "enum": []string{"1d", "1m"}},
								"warmup_days":   map[string]any{"type": "integer"},
								"minute_max_rows": map[string]any{"type": "integer",
									"description": "分钟窗口 Bar 总数上限(默认 300 万,按池大小自动收缩窗口)"},
							},
							"required": []string{"strategy_code", "start_date", "end_date"},
						}},
					}},
					"responses": map[string]any{"202": map[string]any{"description": "已入队"}},
				},
				"get": map[string]any{"summary": "作业列表(概要)", "responses": map[string]any{"200": okResponse()}},
			},
			"/api/backtests/{id}": map[string]any{
				"get": map[string]any{
					"summary": "查询作业状态与结果",
					"parameters": []any{map[string]any{
						"name": "id", "in": "path", "required": true,
						"schema": map[string]any{"type": "string"}}},
					"responses": map[string]any{"200": okResponse()},
				},
			},
			"/healthz": map[string]any{
				"get": map[string]any{"summary": "健康检查(数据集与分区数)",
					"responses": map[string]any{"200": okResponse()}},
			},
		},
	}
}

var docsTemplate = template.Must(template.New("docs").Parse(docsHTML))
