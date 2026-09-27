package query

import (
	"testing"

	"quant-core/internal/schema"
)

// TestPartitionMonthRange 回归:跨月窗口必须覆盖末月分区。
// 曾因从 lo 的日号开始逐月推进而跳过末月(lo=2019-12-29 时 2020-01 被剪掉)。
func TestPartitionMonthRange(t *testing.T) {
	reg, err := schema.Load(`..\..\schemas\datasets.yaml`)
	if err != nil {
		t.Skip("registry unavailable:", err)
	}
	ds, _ := reg.Get("bars_1m")
	timeIdx, _ := ds.FieldIndex("trade_time")

	cases := []struct {
		start, end string
		want       []string
	}{
		{"20191229", "20200102", []string{"2019-12", "2020-01"}},
		{"20240115", "20240210", []string{"2024-01", "2024-02"}},
		{"20240102", "20241231", []string{"2024-01", "2024-12"}},
		{"20200102", "20200102", []string{"2020-01"}},
	}
	for _, tc := range cases {
		lo, err := schema.ParseDate(tc.start)
		if err != nil {
			t.Fatal(err)
		}
		hi, err := schema.ParseDate(tc.end)
		if err != nil {
			t.Fatal(err)
		}
		filter := &Filter{Preds: []Predicate{
			Gte(timeIdx, schema.Timestamp(lo*86400*1_000_000)),
			Lte(timeIdx, schema.Timestamp((hi+1)*86400*1_000_000-1)),
		}}
		accept, err := partitionAccept(ds, filter, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, month := range tc.want {
			values := map[string]string{"market": "hs", "year": month[:4], "month": month[5:]}
			if !accept(values, 2) {
				t.Errorf("%s..%s: 分区 %s 被错误剪枝", tc.start, tc.end, month)
			}
		}
		// 范围外的月份应被剪掉
		values := map[string]string{"market": "hs", "year": "2020", "month": "03"}
		if accept(values, 2) {
			t.Errorf("%s..%s: 分区 2020-03 不应通过", tc.start, tc.end)
		}
	}
}
