package ingest

import (
	"testing"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/schema"
)

// TestConvertSourcePhysicalType 回归:源文件同一字段可能是 DATE(INT32 天)
// 或 TIMESTAMP(INT64),转换必须以物理类型为准。
// 曾因按映射声明(时间戳)解析 DATE 列而把 2022 年公司行动日期写成 1970-01-01。
func TestConvertSourcePhysicalType(t *testing.T) {
	days := int64(19195) // 2022-07-22
	micros := days * 24 * 3600 * 1_000_000

	cases := []struct {
		name    string
		pv      parquet.Value
		fm      FieldMap
		dstType schema.FieldType
		want    int64
	}{
		{"声明时间戳但实际 DATE", parquet.Int32Value(int32(days)),
			FieldMap{Kind: SrcTimestamp}, schema.TypeDate, days},
		{"声明日期且实际 DATE", parquet.Int32Value(int32(days)),
			FieldMap{Kind: SrcDate}, schema.TypeDate, days},
		{"声明日期但实际 TIMESTAMP", parquet.Int64Value(micros),
			FieldMap{Kind: SrcDate}, schema.TypeDate, days},
		{"声明时间戳且实际 TIMESTAMP", parquet.Int64Value(micros),
			FieldMap{Kind: SrcTimestamp}, schema.TypeDate, days},
	}
	for _, tc := range cases {
		got := convertSource(tc.pv, tc.fm, 1, tc.dstType)
		if got.Kind != schema.KindDate || got.I != tc.want {
			t.Errorf("%s: got kind=%v value=%d, want date %d", tc.name, got.Kind, got.I, tc.want)
		}
	}

	// 目标是 timestamp 时,DATE 源按当日零点
	got := convertSource(parquet.Int32Value(int32(days)), FieldMap{Kind: SrcTimestamp}, 1, schema.TypeTimestamp)
	if got.Kind != schema.KindTimestamp || got.I != micros {
		t.Errorf("DATE→TIMESTAMP: got kind=%v value=%d, want %d", got.Kind, got.I, micros)
	}
}
