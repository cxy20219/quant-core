package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
	"quant-core/internal/source/tushare"
)

// mockTushare 模拟 tushare pro 服务,按 api_name 返回固定数据。
func mockTushare(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIName string         `json:"api_name"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeData := func(fields []string, items [][]any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": nil,
				"data": map[string]any{"fields": fields, "items": items, "has_more": false},
			})
		}
		switch req.APIName {
		case "stock_basic":
			writeData(
				[]string{"ts_code", "symbol", "name", "area", "industry", "market", "list_status", "list_date"},
				[][]any{
					{"600000.SH", "600000", "浦发银行", "上海", "银行", "主板", "L", "19991110"},
					{"000001.SZ", "000001", "平安银行", "深圳", "银行", "主板", "L", "19910403"},
					{"600001.SH", "600001", "退市股", "上海", "钢铁", "主板", "D", "19980101"},
				},
			)
		case "trade_cal":
			writeData(
				[]string{"exchange", "cal_date", "is_open", "pretrade_date"},
				[][]any{
					{"SSE", "20240102", float64(1), "20231229"},
					{"SSE", "20240103", float64(1), "20240102"},
				},
			)
		case "stk_limit":
			tradeDate, _ := req.Params["trade_date"].(string)
			if tradeDate != "20240102" {
				writeData([]string{}, nil)
				return
			}
			writeData(
				[]string{"ts_code", "trade_date", "pre_close", "up_limit", "down_limit"},
				[][]any{
					{"600000.SH", "20240102", 7.23, 7.95, 6.51},
				},
			)
		default:
			writeData([]string{}, nil)
		}
	}))
}

func setupImporter(t *testing.T, url string) (*TushareImporter, *lake.Lake) {
	t.Helper()
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	l := lake.New(t.TempDir(), reg)
	client := tushare.NewClient("mock-token")
	client.URL = url
	return &TushareImporter{Client: client, Lake: l, Logger: lake.NewBatchLogger(l.Root)}, l
}

func TestImportSnapshotPartitioned(t *testing.T) {
	srv := mockTushare(t)
	defer srv.Close()
	importer, l := setupImporter(t, srv.URL)

	var spec *TushareSpec
	for _, s := range TushareSpecs() {
		if s.Dataset == "stock_basic" {
			spec = s
		}
	}
	rows, err := importer.Import(context.Background(), spec, ImportOptions{Replace: true})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rows != 3 {
		t.Fatalf("want 3 rows, got %d", rows)
	}

	// stock_basic 无分区:所有行写入数据集根目录下的单一文件
	ds, _ := l.Registry.Get("stock_basic")
	refs, err := l.WalkFiles(ds, nil)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 part file, got %d", len(refs))
	}

	// 用扫描器验证数据可读且字段正确
	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{Dataset: "stock_basic", Columns: []string{"ts_code", "name", "list_status", "list_date"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	seen := map[string]string{}
	for cur.Next() {
		row := cur.Row()
		seen[row[0].S] = row[2].S
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if seen["600000.SH"] != "L" || seen["600001.SH"] != "D" {
		t.Fatalf("unexpected rows: %v", seen)
	}
}

func TestImportDateRange(t *testing.T) {
	srv := mockTushare(t)
	defer srv.Close()
	importer, l := setupImporter(t, srv.URL)

	var spec *TushareSpec
	for _, s := range TushareSpecs() {
		if s.Dataset == "stk_limit" {
			spec = s
		}
	}
	opts := ImportOptions{
		StartDate: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC),
	}
	// 20240102 有数据,20240103 返回空
	rows, err := importer.Import(context.Background(), spec, opts)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rows != 1 {
		t.Fatalf("want 1 row, got %d", rows)
	}

	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{Dataset: "stk_limit", Columns: []string{"ts_code", "trade_date", "up_limit", "down_limit"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	count := 0
	for cur.Next() {
		row := cur.Row()
		if row[0].S != "600000.SH" || row[2].F != 7.95 || row[3].F != 6.51 {
			t.Errorf("row = %+v", row)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("want 1 row, got %d", count)
	}
}

func TestImportSnapshotReplace(t *testing.T) {
	srv := mockTushare(t)
	defer srv.Close()
	importer, l := setupImporter(t, srv.URL)

	var spec *TushareSpec
	for _, s := range TushareSpecs() {
		if s.Dataset == "stock_basic" {
			spec = s
		}
	}
	if _, err := importer.Import(context.Background(), spec, ImportOptions{Replace: true}); err != nil {
		t.Fatalf("import 1: %v", err)
	}
	if _, err := importer.Import(context.Background(), spec, ImportOptions{Replace: true}); err != nil {
		t.Fatalf("import 2: %v", err)
	}
	ds, _ := l.Registry.Get("stock_basic")
	refs, _ := l.WalkFiles(ds, nil)
	// replace 后应只剩一个 part 文件(无分区数据集)
	if len(refs) != 1 {
		t.Fatalf("replace should leave 1 file, got %d", len(refs))
	}
	// 行数不因重复导入而翻倍
	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{Dataset: "stock_basic", Columns: []string{"ts_code"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	count := 0
	for cur.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("replace should keep 3 rows, got %d", count)
	}
}
