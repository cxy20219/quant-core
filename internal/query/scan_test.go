package query

import (
	"path/filepath"
	"testing"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

func loadRegistry(t *testing.T) *schema.Registry {
	t.Helper()
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

func newTestLake(t *testing.T, reg *schema.Registry) *lake.Lake {
	t.Helper()
	return lake.New(t.TempDir(), reg)
}

// dailyRow 构造 bars_daily 的一行,关键字段有值,其余为 null。
func dailyRow(t *testing.T, ds *schema.Dataset, code string, days int64, close float64, vol float64) []schema.Value {
	t.Helper()
	row := make([]schema.Value, len(ds.Fields))
	for i := range row {
		row[i] = schema.NullValue()
	}
	set := func(name string, v schema.Value) {
		i, ok := ds.FieldIndex(name)
		if !ok {
			t.Fatalf("field %s not found", name)
		}
		row[i] = v
	}
	set("ts_code", schema.Str(code))
	set("trade_date", schema.Date(days))
	set("open", schema.Float(close))
	set("high", schema.Float(close + 1))
	set("low", schema.Float(close - 1))
	set("close", schema.Float(close))
	set("pre_close", schema.Float(close - 0.5))
	set("vol", schema.Float(vol))
	set("amount", schema.Float(vol * close / 10))
	set("total_mv", schema.Float(close * 1e6))
	return row
}

func writePartition(t *testing.T, l *lake.Lake, ds *schema.Dataset, values map[string]string, rows [][]schema.Value) {
	t.Helper()
	rel, err := lake.PartitionPath(ds, values)
	if err != nil {
		t.Fatalf("partition path: %v", err)
	}
	dir := filepath.Join(l.Dir(ds), rel)
	pw, err := lake.NewPartWriter(ds, dir)
	if err != nil {
		t.Fatalf("new part writer: %v", err)
	}
	for _, r := range rows {
		if err := pw.WriteRow(r); err != nil {
			t.Fatalf("write row: %v", err)
		}
	}
	if _, err := pw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
}

func mustDate(t *testing.T, s string) int64 {
	t.Helper()
	days, err := schema.ParseDate(s)
	if err != nil {
		t.Fatalf("parse date %s: %v", s, err)
	}
	return days
}

// collect 扫描并返回全部行(作为 [][]schema.Value)。
func collect(t *testing.T, sc *Scanner, req Request) ([][]schema.Value, Stats) {
	t.Helper()
	cur, err := sc.Open(req)
	if err != nil {
		t.Fatalf("open cursor: %v", err)
	}
	defer cur.Close()
	var rows [][]schema.Value
	for cur.Next() {
		rows = append(rows, cur.Row())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}
	return rows, cur.Stats()
}

func TestRoundTripDaily(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")

	day1 := mustDate(t, "20230103")
	day2 := mustDate(t, "20230104")
	day3 := mustDate(t, "20230105")
	writePartition(t, l, ds, map[string]string{"year": "2023"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", day1, 10.0, 1000),
		dailyRow(t, ds, "600000.SH", day2, 10.5, 2000),
		dailyRow(t, ds, "000001.SZ", day3, 20.0, 3000),
	})

	sc := NewScanner(l)
	rows, stats := collect(t, sc, Request{Dataset: "bars_daily"})
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	if stats.FilesOpened != 1 {
		t.Fatalf("want 1 file opened, got %d", stats.FilesOpened)
	}

	// 全部字段输出,校验关键值
	tsCodeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	closeIdx, _ := ds.FieldIndex("close")
	peIdx, _ := ds.FieldIndex("pe")

	first := rows[0]
	if first[tsCodeIdx].S != "600000.SH" {
		t.Errorf("ts_code = %q", first[tsCodeIdx].S)
	}
	if first[dateIdx].I != day1 || first[dateIdx].Kind != schema.KindDate {
		t.Errorf("trade_date = %+v, want %d", first[dateIdx], day1)
	}
	if first[closeIdx].F != 10.0 {
		t.Errorf("close = %v", first[closeIdx].F)
	}
	if !first[peIdx].IsNull() {
		t.Errorf("pe should be null, got %+v", first[peIdx])
	}
}

func TestColumnProjection(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")

	day1 := mustDate(t, "20240102")
	writePartition(t, l, ds, map[string]string{"year": "2024"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", day1, 12.0, 500),
	})

	sc := NewScanner(l)
	rows, _ := collect(t, sc, Request{Dataset: "bars_daily", Columns: []string{"ts_code", "close"}})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Fatalf("want 2 columns, got %d", len(rows[0]))
	}
	if rows[0][0].S != "600000.SH" || rows[0][1].F != 12.0 {
		t.Errorf("row = %+v", rows[0])
	}
}

func TestFilterByCodeAndDateRange(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")

	codeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")

	writePartition(t, l, ds, map[string]string{"year": "2023"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230103"), 10.0, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230104"), 10.5, 2000),
		dailyRow(t, ds, "000001.SZ", mustDate(t, "20230104"), 20.0, 3000),
	})
	writePartition(t, l, ds, map[string]string{"year": "2024"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", mustDate(t, "20240102"), 11.0, 1500),
	})

	sc := NewScanner(l)
	// 只查 2023 年的 600000.SH
	rows, stats := collect(t, sc, Request{
		Dataset: "bars_daily",
		Filter: &Filter{Preds: []Predicate{
			In(codeIdx, schema.Str("600000.SH")),
			Gte(dateIdx, schema.Date(mustDate(t, "20230101"))),
			Lte(dateIdx, schema.Date(mustDate(t, "20231231"))),
		}},
	})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if stats.FilesOpened != 1 {
		t.Errorf("partition pruning failed: %d files opened, want 1", stats.FilesOpened)
	}
}

func TestLimitOffset(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")

	writePartition(t, l, ds, map[string]string{"year": "2023"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230103"), 10.0, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230104"), 10.5, 2000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230105"), 11.0, 3000),
	})

	sc := NewScanner(l)
	rows, _ := collect(t, sc, Request{Dataset: "bars_daily", Limit: 2})
	if len(rows) != 2 {
		t.Fatalf("limit: want 2 rows, got %d", len(rows))
	}
	rows, _ = collect(t, sc, Request{Dataset: "bars_daily", Offset: 1, Limit: 5})
	if len(rows) != 2 {
		t.Fatalf("offset: want 2 rows, got %d", len(rows))
	}
	closeIdx, _ := ds.FieldIndex("close")
	if rows[0][closeIdx].F != 10.5 {
		t.Errorf("offset row close = %v, want 10.5", rows[0][closeIdx].F)
	}
}

func TestRowGroupSkipping(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")
	// 每 2 行一个行组,便于验证统计跳过
	ds.Writer.RowGroupRows = 2
	ds.Writer.TargetFileRows = 1_000_000

	codeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")

	// 2024 分区 6 行,组内按 code 排序(600000.SH×4 / 000001.SZ×2)
	writePartition(t, l, ds, map[string]string{"year": "2024"}, [][]schema.Value{
		dailyRow(t, ds, "000001.SZ", mustDate(t, "20240102"), 20.0, 3000),
		dailyRow(t, ds, "000001.SZ", mustDate(t, "20240103"), 20.5, 3000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20240102"), 10.0, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20240103"), 10.5, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20240104"), 11.0, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20240105"), 11.5, 1000),
	})

	sc := NewScanner(l)
	rows, stats := collect(t, sc, Request{
		Dataset: "bars_daily",
		Filter:  &Filter{Preds: []Predicate{In(codeIdx, schema.Str("600000.SH"))}},
	})
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d", len(rows))
	}
	_ = dateIdx
	if stats.RowGroupsSkipped == 0 {
		t.Errorf("expected row groups to be skipped, stats=%+v", stats)
	}
	if stats.RowGroupsRead == 0 {
		t.Errorf("expected row groups to be read, stats=%+v", stats)
	}
}

func TestSingleDayFilter(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")

	codeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	day := mustDate(t, "20230104")

	writePartition(t, l, ds, map[string]string{"year": "2023"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230103"), 10.0, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230104"), 10.5, 2000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230105"), 11.0, 3000),
	})

	sc := NewScanner(l)
	rows, stats := collect(t, sc, Request{
		Dataset: "bars_daily",
		Filter: &Filter{Preds: []Predicate{
			In(codeIdx, schema.Str("600000.SH")),
			Eq(dateIdx, schema.Date(day)),
		}},
	})
	if len(rows) != 1 {
		t.Fatalf("single-day filter: want 1 row, got %d (files=%d)", len(rows), stats.FilesOpened)
	}
	if stats.FilesOpened != 1 {
		t.Errorf("single-day filter pruned its own partition: files=%d", stats.FilesOpened)
	}
	closeIdx, _ := ds.FieldIndex("close")
	if rows[0][closeIdx].F != 10.5 {
		t.Errorf("close = %v, want 10.5", rows[0][closeIdx].F)
	}
}

func TestMetaCacheReuse(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_daily")

	codeIdx, _ := ds.FieldIndex("ts_code")
	writePartition(t, l, ds, map[string]string{"year": "2023"}, [][]schema.Value{
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230103"), 10.0, 1000),
		dailyRow(t, ds, "600000.SH", mustDate(t, "20230104"), 10.5, 2000),
		dailyRow(t, ds, "000001.SZ", mustDate(t, "20230104"), 20.0, 3000),
	})

	sc := NewCachedScanner(l, 16)
	filter := &Filter{Preds: []Predicate{In(codeIdx, schema.Str("600000.SH"))}}

	// 第一次扫描:构建元数据缓存
	rows, stats := collect(t, sc, Request{Dataset: "bars_daily", Filter: filter})
	if len(rows) != 2 {
		t.Fatalf("first scan: want 2 rows, got %d", len(rows))
	}
	if stats.MetaBuilt == 0 {
		t.Error("first scan should build metadata")
	}
	if sc.Meta.Len() == 0 {
		t.Error("metadata cache should be populated")
	}

	// 第二次扫描:应命中缓存,不再构建元数据
	if _, stats2 := collect(t, sc, Request{Dataset: "bars_daily", Filter: filter}); stats2.MetaBuilt != 0 {
		t.Errorf("second scan rebuilt metadata: %+v", stats2)
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	reg := loadRegistry(t)
	l := newTestLake(t, reg)
	ds, _ := reg.Get("bars_1m")

	tsIdx, _ := ds.FieldIndex("trade_time")
	codeIdx, _ := ds.FieldIndex("ts_code")

	// 2024-01-02 09:31:00 UTC == micros
	micros := schema.MicrosFromTime(schema.TimeFromDays(mustDate(t, "20240102")).Add(9*3600 + 31*60))
	row := make([]schema.Value, len(ds.Fields))
	for i := range row {
		row[i] = schema.NullValue()
	}
	row[tsIdx] = schema.Timestamp(micros)
	row[codeIdx] = schema.Str("600000.SH")
	closeIdx, _ := ds.FieldIndex("close")
	row[closeIdx] = schema.Float(10.25)

	writePartition(t, l, ds, map[string]string{"market": "hs", "year": "2024", "month": "01"}, [][]schema.Value{row})

	sc := NewScanner(l)
	rows, _ := collect(t, sc, Request{Dataset: "bars_1m"})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0][tsIdx].I != micros || rows[0][tsIdx].Kind != schema.KindTimestamp {
		t.Errorf("trade_time = %+v, want micros %d", rows[0][tsIdx], micros)
	}
	if rows[0][closeIdx].F != 10.25 {
		t.Errorf("close = %v", rows[0][closeIdx].F)
	}
	_ = codeIdx

	// 月份分区剪枝
	rows, stats := collect(t, sc, Request{
		Dataset: "bars_1m",
		Filter: &Filter{Preds: []Predicate{
			Gte(tsIdx, schema.Timestamp(schema.MicrosFromTime(schema.TimeFromDays(mustDate(t, "20240201"))))),
		}},
	})
	if len(rows) != 0 {
		t.Errorf("want 0 rows for Feb range, got %d", len(rows))
	}
	if stats.FilesOpened != 0 {
		t.Errorf("month partition pruning failed: opened %d files", stats.FilesOpened)
	}
}
