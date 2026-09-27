package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
	"quant-core/internal/source"
)

// fakeSource 是测试用的数据源插件。
type fakeSource struct {
	name    string
	results map[string]*source.Result // key: api|参数摘要
	calls   []string
	failOn  string
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Kind() string { return "fake" }
func (f *fakeSource) Close() error { return nil }

func (f *fakeSource) Call(_ context.Context, api string, params map[string]any, _ string) (*source.Result, error) {
	key := api + "|" + summarize(params)
	f.calls = append(f.calls, key)
	if f.failOn != "" && strings.Contains(key, f.failOn) {
		return nil, fmt.Errorf("simulated failure for %s", key)
	}
	if res, ok := f.results[key]; ok {
		return res, nil
	}
	return &source.Result{Fields: []string{}}, nil
}

func summarize(params map[string]any) string {
	keys := []string{"list_status", "exchange", "market", "start_date", "end_date", "trade_date"}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := params[k]; ok && v != nil && fmt.Sprint(v) != "" {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, ",")
}

func setupImporter(t *testing.T, src source.Source) (*Importer, *lake.Lake, *schema.Registry) {
	t.Helper()
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	l := lake.New(t.TempDir(), reg)
	return &Importer{Source: src, Lake: l, Logger: lake.NewBatchLogger(l.Root)}, l, reg
}

// stockBasicRow 生成 stock_basic 的一行测试数据。
func stockBasicRow(code, name, status string) []any {
	return []any{code, strings.Split(code, ".")[0], name, "上海", "银行", name + "股份", "", "", "主板", "SSE", "CNY", status, "19991110", nil, "S", "", ""}
}

func TestImportSnapshotWithSlices(t *testing.T) {
	fields := []string{"ts_code", "symbol", "name", "area", "industry", "fullname", "enname", "cnspell", "market", "exchange", "curr_type", "list_status", "list_date", "delist_date", "is_hs", "act_name", "act_ent_type"}
	src := &fakeSource{
		name: "fake",
		results: map[string]*source.Result{
			"stock_basic|list_status=L": {
				Fields: fields,
				Items: [][]any{
					stockBasicRow("600000.SH", "浦发银行", "L"),
					stockBasicRow("600001.SH", "邯郸钢铁", "L"),
				},
				Count: 2, Limit: 10000,
			},
			"stock_basic|list_status=D": {
				Fields: fields,
				Items:  [][]any{stockBasicRow("000001.SZ", "平安银行", "L")},
				Count:  1, Limit: 10000,
			},
			"stock_basic|list_status=P": {
				Fields: fields,
				Items:  [][]any{stockBasicRow("600002.SH", "退市股", "D")},
				Count:  1, Limit: 10000,
			},
		},
	}
	importer, l, reg := setupImporter(t, src)
	spec, _ := SpecByName("stock_basic")

	rows, err := importer.Import(context.Background(), spec, ImportOptions{Replace: true})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rows != 4 {
		t.Fatalf("want 4 rows, got %d", rows)
	}
	// 数据可读
	ds, _ := reg.Get("stock_basic")
	sc := query.NewCachedScanner(l, 8)
	cur, err := sc.Open(query.Request{Dataset: ds.Name, Columns: []string{"ts_code", "list_status"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	seen := map[string]string{}
	for cur.Next() {
		row := cur.Row()
		seen[row[0].S] = row[1].S
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if len(seen) != 4 || seen["600002.SH"] != "D" {
		t.Fatalf("rows = %v", seen)
	}
	if len(src.calls) != 3 { // 3 个 list_status 切片
		t.Fatalf("calls = %d: %v", len(src.calls), src.calls)
	}
}

func TestImportMonthRange(t *testing.T) {
	// 用自定义规格测试 month-range 模式(内置规格当前只有 namechange/suspend_d/index_daily 使用)
	src := &fakeSource{
		name: "fake",
		results: map[string]*source.Result{
			"namechange|start_date=20240101,end_date=20240131": {
				Fields: []string{"ts_code", "name", "start_date", "end_date", "ann_date", "change_reason"},
				Items: [][]any{
					{"600000.SH", "浦发银行", "20240102", nil, "20240102", "改名字"},
					{"000001.SZ", "平安银行", "20240103", nil, "20240103", "改名字"},
				},
				Count: 2, Limit: 10000,
			},
			"namechange|start_date=20240201,end_date=20240229": {
				Fields: []string{"ts_code", "name", "start_date", "end_date", "ann_date", "change_reason"},
				Items:  [][]any{{"600000.SH", "浦发银行", "20240201", nil, "20240201", "改名字"}},
				Count:  1, Limit: 10000,
			},
		},
	}
	importer, l, _ := setupImporter(t, src)
	spec, _ := SpecByName("namechange")
	if spec.Mode != ModeMonthRange {
		t.Fatalf("namechange 应使用 month-range 模式,实际 %s", spec.Mode)
	}

	opts := ImportOptions{
		StartDate: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
	}
	rows, err := importer.Import(context.Background(), spec, opts)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rows != 3 {
		t.Fatalf("want 3 rows, got %d", rows)
	}
	if len(src.calls) != 2 {
		t.Fatalf("calls = %v", src.calls)
	}

	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{Dataset: "namechange", Columns: []string{"ts_code", "start_date"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	count := 0
	for cur.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("want 3 rows scanned, got %d", count)
	}
}

func TestImportDateRange(t *testing.T) {
	// stk_limit 使用逐日模式(单日全市场行数大,月度窗口会触发中转站截断)
	src := &fakeSource{
		name: "fake",
		results: map[string]*source.Result{
			"stk_limit|trade_date=20240102": {
				Fields: []string{"ts_code", "trade_date", "pre_close", "up_limit", "down_limit"},
				Items: [][]any{
					{"600000.SH", "20240102", 7.23, 7.95, 6.51},
					{"000001.SZ", "20240102", 9.9, 10.89, 8.91},
				},
				Count: 2, Limit: 10000,
			},
		},
	}
	importer, l, _ := setupImporter(t, src)
	spec, _ := SpecByName("stk_limit")
	if spec.Mode != ModeDateRange {
		t.Fatalf("stk_limit 应使用 date-range 模式,实际 %s", spec.Mode)
	}
	opts := ImportOptions{
		StartDate: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC),
	}
	rows, err := importer.Import(context.Background(), spec, opts)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if rows != 2 {
		t.Fatalf("want 2 rows, got %d", rows)
	}
	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{Dataset: "stk_limit", Columns: []string{"ts_code", "trade_date", "up_limit"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	count := 0
	for cur.Next() {
		count++
	}
	if count != 2 {
		t.Fatalf("want 2 rows scanned, got %d", count)
	}
}

func TestConvertSourceValueNumericDates(t *testing.T) {
	// stock_basic 的 list_date 在部分中转站是数字(19991110),index_basic 是字符串;
	// 两种都必须正确解析,不能静默置空。
	cases := []struct {
		name   string
		raw    any
		expect string // FormatDate 结果,空串表示应为 null
	}{
		{"numeric date", float64(19991110), "19991110"},
		{"string date", "19991110", "19991110"},
		{"string date dashed", "1999-11-10", "19991110"},
		{"zero numeric", float64(0), ""},
		{"empty string", "", ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		v, err := convertSourceValue(tc.raw, schema.TypeDate)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if tc.expect == "" {
			if !v.IsNull() {
				t.Errorf("%s: want null, got %+v", tc.name, v)
			}
			continue
		}
		if v.IsNull() {
			t.Errorf("%s: got null, want %s", tc.name, tc.expect)
			continue
		}
		if got := schema.FormatDate(v.I); got != tc.expect {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.expect)
		}
	}

	// 时间戳同理:数字 YYYYMMDDHHMMSS 与字符串都要支持
	ts, err := convertSourceValue(float64(20240102093000), schema.TypeTimestamp)
	if err != nil {
		t.Fatalf("timestamp numeric: %v", err)
	}
	if got := schema.FormatTimestamp(ts.I); got != "2024-01-02 09:30:00" {
		t.Errorf("timestamp numeric: got %s", got)
	}
}

func TestImportDetectsTruncationByCount(t *testing.T) {
	fields := []string{"exchange", "cal_date", "is_open", "pretrade_date"}
	src := &fakeSource{
		name: "fake",
		results: map[string]*source.Result{
			"trade_cal|exchange=SSE,start_date=19900101,end_date=19901231": {
				Fields: fields,
				Items:  [][]any{{"SSE", "19901219", float64(1), nil}},
				Count:  5000, // 源声明 5000 条但只返回 1 条
				Limit:  10000,
			},
		},
	}
	importer, _, _ := setupImporter(t, src)
	spec, _ := SpecByName("trade_cal")

	_, err := importer.Import(context.Background(), spec, ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "截断") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

func TestImportDetectsTruncationByLimit(t *testing.T) {
	// 行数恰好等于本次 limit:中转站可能把 count 与行数一起截断,必须靠触顶检测暴露
	fields := []string{"exchange", "cal_date", "is_open", "pretrade_date"}
	items := make([][]any, 5000)
	for i := range items {
		items[i] = []any{"SSE", "20240102", float64(1), nil}
	}
	src := &fakeSource{
		name: "fake",
		results: map[string]*source.Result{
			"trade_cal|exchange=SSE,start_date=19900101,end_date=19901231": {
				Fields: fields,
				Items:  items,
				Count:  5000, // count 与行数一致,但等于 limit
				Limit:  5000,
			},
		},
	}
	importer, _, _ := setupImporter(t, src)
	spec, _ := SpecByName("trade_cal")

	_, err := importer.Import(context.Background(), spec, ImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "恰好等于单次上限") {
		t.Fatalf("expected hit-limit error, got %v", err)
	}
}

func TestImportFallbackChain(t *testing.T) {
	fields := []string{"exchange", "cal_date", "is_open", "pretrade_date"}
	key := "trade_cal|exchange=SSE,start_date=19900101,end_date=19901231"
	primary := &fakeSource{name: "primary", failOn: key}
	backup := &fakeSource{name: "backup", results: map[string]*source.Result{
		key: {Fields: fields, Items: [][]any{{"SSE", "19901219", float64(1), nil}}, Count: 1, Limit: 10000},
	}}

	// 组装降级链(与 cmd/quantd 一致)
	fb, err := source.NewFallback("primary→backup", []source.Source{primary, backup})
	if err != nil {
		t.Fatal(err)
	}
	importer, _, _ := setupImporter(t, fb)
	spec, _ := SpecByName("trade_cal")
	rows, err := importer.Import(context.Background(), spec, ImportOptions{})
	if err != nil {
		t.Fatalf("fallback import: %v", err)
	}
	if rows == 0 {
		t.Fatal("expected rows from backup source")
	}
	if len(primary.calls) == 0 || len(backup.calls) == 0 {
		t.Fatalf("both sources should be called: primary=%v backup=%v", primary.calls, backup.calls)
	}
}
