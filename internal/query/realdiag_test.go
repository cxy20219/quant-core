package query

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// TestRealFileSkipPlan 针对真实 2011 日线文件检查跳过计划的正确性。
// 仅在本机存在 D:\quant-lake 时执行;不存在时跳过。
func TestRealFileSkipPlan(t *testing.T) {
	lakeRoot := `D:\quant-lake`
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Skip("registry unavailable:", err)
	}
	ds, _ := reg.Get("bars_daily")
	l := lake.New(lakeRoot, reg)
	refs, err := l.WalkFiles(ds, func(values map[string]string, depth int) bool {
		return values["year"] == "2011"
	})
	if err != nil || len(refs) == 0 {
		t.Skip("2011 partition not found")
	}
	path := refs[0].Path
	t.Logf("file: %s", path)

	fh, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	pf, err := parquet.OpenFile(fh, st.Size())
	if err != nil {
		t.Fatalf("parquet open: %v", err)
	}
	sm := buildFileMeta(pf, st.Size(), st.ModTime().UnixNano())

	tsIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	start, _ := schema.ParseDate("20110101")
	end, _ := schema.ParseDate("20111231")
	filter := &Filter{Preds: []Predicate{
		In(tsIdx, schema.Str("600000.SH")),
		Gte(dateIdx, schema.Date(start)),
		Lte(dateIdx, schema.Date(end)),
	}}
	plan := rgSkipPlan(sm, ds, filter)
	read, skip := 0, 0
	for i, s := range plan {
		if s {
			skip++
			continue
		}
		read++
		stats := sm.groups[i].cols["ts_code"]
		t.Logf("read rg[%d] ts_code: %q..%q rows=%d", i, string(stats.min.ByteArray()), string(stats.max.ByteArray()), sm.groups[i].numRows)
	}
	t.Logf("groups: read=%d skip=%d total=%d", read, skip, len(plan))
	if read > 5 {
		t.Errorf("expected <=5 row groups for a single code, got %d", read)
	}

	// 全链路扫描:验证 scanner 使用的计划一致
	sc := NewCachedScanner(l, 16)
	sc.Debug = true
	cur, err := sc.Open(Request{
		Dataset: "bars_daily",
		Filter:  filter,
		Columns: []string{"ts_code", "trade_date", "close"},
	})
	if err != nil {
		t.Fatalf("scan open: %v", err)
	}
	defer cur.Close()
	rows := 0
	for cur.Next() {
		rows++
	}
	t.Logf("scanner: rows=%d stats=%+v", rows, cur.Stats())
	if got := cur.Stats().RowGroupsRead; got > 5 {
		t.Errorf("scanner read %d row groups, want <=5", got)
	}
}
