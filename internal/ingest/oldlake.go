package ingest

import (
	"fmt"
	"time"
)

func batchID(dataset string, seq int) string {
	return fmt.Sprintf("%s-%s-%04d", dataset, time.Now().UTC().Format("20060102T150405"), seq)
}

func nowUTC() time.Time { return time.Now().UTC() }

// OldLakeMigrations 返回旧湖(quant-data/quant-store)到新湖的全部迁移定义。
// 旧湖字段量纲已是 tushare 口径(vol=手、amount=千元),只有字段名与少量单位差异。
func OldLakeMigrations() []*Migration {
	return []*Migration{
		{
			Dataset: "bars_daily",
			SrcDir:  "bars_daily",
			Fields: []FieldMap{
				{Src: "code", Dst: "ts_code", Kind: SrcString},
				{Src: "date", Dst: "trade_date", Kind: SrcDate},
				{Src: "open", Dst: "open", Kind: SrcFloat},
				{Src: "high", Dst: "high", Kind: SrcFloat},
				{Src: "low", Dst: "low", Kind: SrcFloat},
				{Src: "close", Dst: "close", Kind: SrcFloat},
				{Src: "pre_close", Dst: "pre_close", Kind: SrcFloat},
				{Src: "change", Dst: "change", Kind: SrcFloat},
				{Src: "pct_chg", Dst: "pct_chg", Kind: SrcFloat},
				{Src: "volume", Dst: "vol", Kind: SrcFloat},
				{Src: "amount", Dst: "amount", Kind: SrcFloat},
				{Src: "turnover", Dst: "turnover_rate", Kind: SrcFloat},
				{Src: "turnover_free", Dst: "turnover_rate_f", Kind: SrcFloat},
				{Src: "volume_ratio", Dst: "volume_ratio", Kind: SrcFloat},
				{Src: "pe", Dst: "pe", Kind: SrcFloat},
				{Src: "pe_ttm", Dst: "pe_ttm", Kind: SrcFloat},
				{Src: "pb", Dst: "pb", Kind: SrcFloat},
				{Src: "ps", Dst: "ps", Kind: SrcFloat},
				{Src: "ps_ttm", Dst: "ps_ttm", Kind: SrcFloat},
				{Src: "dv_yield", Dst: "dv_ratio", Kind: SrcFloat},
				{Src: "dv_ttm", Dst: "dv_ttm", Kind: SrcFloat},
				{Src: "total_share", Dst: "total_share", Kind: SrcFloat},
				{Src: "float_share", Dst: "float_share", Kind: SrcFloat},
				{Src: "free_share", Dst: "free_share", Kind: SrcFloat},
				{Src: "total_mv", Dst: "total_mv", Kind: SrcFloat},
				{Src: "circ_mv", Dst: "circ_mv", Kind: SrcFloat},
			},
		},
		{
			Dataset: "bars_1m",
			SrcDir:  "bars_1m",
			Fields: []FieldMap{
				{Src: "code", Dst: "ts_code", Kind: SrcString},
				{Src: "datetime", Dst: "trade_time", Kind: SrcTimestamp},
				{Src: "date", Dst: "", Kind: SrcDate}, // 丢弃,date 见分区
				{Src: "open", Dst: "open", Kind: SrcFloat},
				{Src: "high", Dst: "high", Kind: SrcFloat},
				{Src: "low", Dst: "low", Kind: SrcFloat},
				{Src: "close", Dst: "close", Kind: SrcFloat},
				{Src: "volume", Dst: "vol", Kind: SrcFloat},          // 手
				{Src: "amount", Dst: "amount", Kind: SrcFloat, Scale: 0.001}, // 元 -> 千元
			},
		},
		{
			Dataset: "adj_factor",
			SrcDir:  "adj_factor",
			Fields: []FieldMap{
				{Src: "code", Dst: "ts_code", Kind: SrcString},
				{Src: "date", Dst: "trade_date", Kind: SrcDate},
				{Src: "factor", Dst: "adj_factor", Kind: SrcFloat},
				{Src: "factor_type", Dst: "", Kind: SrcString}, // 见分区
			},
		},
		{
			Dataset: "corporate_actions",
			SrcDir:  "corporate_actions",
			Fields: []FieldMap{
				{Src: "code", Dst: "ts_code", Kind: SrcString},
				{Src: "date", Dst: "ex_date", Kind: SrcTimestamp}, // timestamp -> date
				{Src: "allotted_ps", Dst: "allotted_ps", Kind: SrcFloat},
				{Src: "rationed_ps", Dst: "rationed_ps", Kind: SrcFloat},
				{Src: "rationed_px", Dst: "rationed_px", Kind: SrcFloat},
				{Src: "bonus_ps", Dst: "bonus_ps", Kind: SrcFloat},
				{Src: "exer_forward_a", Dst: "exer_forward_a", Kind: SrcFloat},
				{Src: "exer_forward_b", Dst: "exer_forward_b", Kind: SrcFloat},
				{Src: "exer_backward_a", Dst: "exer_backward_a", Kind: SrcFloat},
				{Src: "exer_backward_b", Dst: "exer_backward_b", Kind: SrcFloat},
			},
		},
	}
}
