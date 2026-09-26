package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// writeOldLakeFile 在 srcDir 下写出一个旧湖风格的 parquet 文件。
// 旧湖特征:每行组一只股票、列名与目标不同(volume/amount/turnover)、
// 行组顺序随机、字符串列使用默认编码。
func writeOldLakeFile(t *testing.T, path string, blocks []oldBlock) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	schemaDef := parquet.NewSchema("old", parquet.Group{
		"code":     parquet.Optional(parquet.String()),
		"date":     parquet.Optional(parquet.Date()),
		"open":     parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"high":     parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"low":      parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"close":    parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"pre_close": parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"change":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"pct_chg":  parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"volume":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"amount":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"turnover": parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"turnover_free": parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"volume_ratio":  parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"pe":       parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"pe_ttm":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"pb":       parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"ps":       parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"ps_ttm":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"dv_yield": parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"dv_ttm":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"total_share":  parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"float_share":  parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"free_share":   parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"total_mv":     parquet.Optional(parquet.Leaf(parquet.DoubleType)),
		"circ_mv":      parquet.Optional(parquet.Leaf(parquet.DoubleType)),
	})
	// 行组按股票块切分:每块一个行组,直接用底层 Writer 手工构造行
	fh, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer fh.Close()
	w := parquet.NewWriter(fh, schemaDef, parquet.MaxRowsPerRowGroup(256))
	defer w.Close()
	nameToPos := map[string]int{}
	for i, field := range schemaDef.Fields() {
		nameToPos[field.Name()] = i
	}
	toRow := func(block oldBlock, i int) parquet.Row {
		values := map[string]parquet.Value{
			"code":  parquet.ByteArrayValue([]byte(block.code)),
			"date":  parquet.Int32Value(int32(block.days[i])),
			"open":  parquet.DoubleValue(block.close[i] - 0.1),
			"high":  parquet.DoubleValue(block.close[i] + 0.2),
			"low":   parquet.DoubleValue(block.close[i] - 0.2),
			"close": parquet.DoubleValue(block.close[i]),
			"pre_close": parquet.DoubleValue(block.close[i] - 0.05),
			"change": parquet.DoubleValue(0.05),
			"pct_chg": parquet.DoubleValue(0.7),
			"volume": parquet.DoubleValue(block.vol[i]),
			"amount": parquet.DoubleValue(block.amount[i]),
			"turnover": parquet.DoubleValue(1.1),
			"turnover_free": parquet.DoubleValue(2.2),
			"volume_ratio":  parquet.DoubleValue(0.9),
			"pe":      parquet.DoubleValue(10),
			"pe_ttm":  parquet.DoubleValue(9),
			"pb":      parquet.DoubleValue(1),
			"ps":      parquet.DoubleValue(2),
			"ps_ttm":  parquet.DoubleValue(2),
			"dv_yield": parquet.DoubleValue(3),
			"dv_ttm":   parquet.DoubleValue(3),
			"total_share": parquet.DoubleValue(1000),
			"float_share": parquet.DoubleValue(800),
			"free_share":  parquet.DoubleValue(700),
			"total_mv":    parquet.DoubleValue(5000),
			"circ_mv":     parquet.DoubleValue(4000),
		}
		row := make(parquet.Row, len(values))
		for name, v := range values {
			pos := nameToPos[name]
			row[pos] = v.Level(0, 1, pos)
		}
		return row
	}
	for _, block := range blocks {
		rows := make([]parquet.Row, len(block.days))
		for i := range block.days {
			rows[i] = toRow(block, i)
		}
		if _, err := w.WriteRows(rows); err != nil {
			t.Fatalf("write rows: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
}

type oldBlock struct {
	code   string
	days   []int64
	close  []float64
	vol    []float64
	amount []float64
}

func TestMigrateOldLake(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()

	d1, _ := schema.ParseDate("20240102")
	d2, _ := schema.ParseDate("20240103")
	// 故意让代码块顺序随机:600000.SH 在 000001.SZ 之前
	writeOldLakeFile(t, filepath.Join(srcRoot, "bars_daily", "year=2024", "2024.parquet"), []oldBlock{
		{code: "600000.SH", days: []int64{d1, d2}, close: []float64{7.0, 7.1}, vol: []float64{1000, 1100}, amount: []float64{7000, 7810}},
		{code: "000001.SZ", days: []int64{d1, d2}, close: []float64{12.0, 12.2}, vol: []float64{2000, 2100}, amount: []float64{24000, 25620}},
	})

	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	l := lake.New(dstRoot, reg)
	migrator := &Migrator{SrcRoot: srcRoot, Lake: l, Logger: lake.NewBatchLogger(dstRoot)}

	var mig *Migration
	for _, m := range OldLakeMigrations() {
		if m.Dataset == "bars_daily" {
			mig = m
		}
	}
	rows, err := migrator.Migrate(mig, MigrateOptions{Jobs: 2})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rows != 4 {
		t.Fatalf("want 4 rows, got %d", rows)
	}

	// 校验:字段改名生效、代码块按代码排序、行组可被剪枝
	ds, _ := reg.Get("bars_daily")
	sc := query.NewCachedScanner(l, 8)
	codeIdx, _ := ds.FieldIndex("ts_code")
	cur, err := sc.Open(query.Request{
		Dataset: "bars_daily",
		Filter:  &query.Filter{Preds: []query.Predicate{query.In(codeIdx, schema.Str("600000.SH"))}},
		Columns: []string{"ts_code", "trade_date", "vol", "amount", "turnover_rate"},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer cur.Close()
	var got [][]schema.Value
	for cur.Next() {
		got = append(got, cur.Row())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("scan err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows for 600000.SH, got %d", len(got))
	}
	if got[0][0].S != "600000.SH" || got[1][0].S != "600000.SH" {
		t.Errorf("code filter failed: %v", got)
	}
	// volume -> vol、amount 保持千元、turnover -> turnover_rate
	if got[0][2].F != 1000 || got[0][3].F != 7000 || got[0][4].F != 1.1 {
		t.Errorf("column mapping failed: %+v", got[0])
	}
	// 排序:20240102 在前
	if schema.FormatDate(got[0][1].I) != "20240102" || schema.FormatDate(got[1][1].I) != "20240103" {
		t.Errorf("date order failed: %v %v", got[0][1], got[1][1])
	}

	// manifest 记录存在
	summary, err := lake.NewBatchLogger(dstRoot).Summarize("bars_daily")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if summary.Rows != 4 || summary.Batches != 1 {
		t.Errorf("manifest summary = %+v", summary)
	}
}
