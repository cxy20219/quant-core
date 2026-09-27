package query

import (
	"testing"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// TestCrossYearScan 回归:跨年时间窗口必须读到新年份首日的数据。
// 依赖本机数据湖 D:\quant-lake(不存在时跳过)。
func TestCrossYearScan(t *testing.T) {
	reg, err := schema.Load(`..\..\schemas\datasets.yaml`)
	if err != nil {
		t.Skip("registry unavailable:", err)
	}
	ds, _ := reg.Get("bars_1m")
	timeIdx, _ := ds.FieldIndex("trade_time")
	codeIdx, _ := ds.FieldIndex("ts_code")
	day, _ := schema.ParseDate("20200102")
	lo, _ := schema.ParseDate("20191229")

	l := lake.New(`D:\quant-lake`, reg)
	sc := NewScanner(l)
	cur, err := sc.Open(Request{
		Dataset: "bars_1m",
		Columns: []string{"ts_code", "trade_time"},
		Filter: &Filter{Preds: []Predicate{
			In(codeIdx, schema.Str("600570.SH")),
			Gte(timeIdx, schema.Timestamp(lo*86400*1_000_000)),
			Lte(timeIdx, schema.Timestamp((day+1)*86400*1_000_000-1)),
		}},
	})
	if err != nil {
		t.Skip("lake unavailable:", err)
	}
	defer cur.Close()
	rows := 0
	lastDay := int64(0)
	for cur.Next() {
		row := cur.Row()
		rows++
		lastDay = row[1].I / (86400 * 1_000_000)
	}
	if err := cur.Err(); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Skip("lake has no 600570.SH minute data in range")
	}
	if lastDay != day {
		t.Errorf("跨年窗口末行日期 = %s,期望 %s(共 %d 行)", schema.FormatDateISO(lastDay), schema.FormatDateISO(day), rows)
	}
}
