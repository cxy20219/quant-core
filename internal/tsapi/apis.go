package tsapi

import (
	"fmt"
	"strings"
	"time"

	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// DefaultAPIs 返回内置的 tushare 兼容接口表。
func DefaultAPIs() map[string]*API {
	return map[string]*API{
		"daily": {
			Name:         "daily",
			Dataset:      "bars_daily",
			BuildFilter:  codeDateFilter,
			SelectFields: []string{"ts_code", "trade_date", "open", "high", "low", "close", "pre_close", "change", "pct_chg", "vol", "amount"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"daily_basic": {
			Name:        "daily_basic",
			Dataset:     "bars_daily",
			BuildFilter: codeDateFilter,
			SelectFields: []string{
				"ts_code", "trade_date", "close", "turnover_rate", "turnover_rate_f", "volume_ratio",
				"pe", "pe_ttm", "pb", "ps", "ps_ttm", "dv_ratio", "dv_ttm",
				"total_share", "float_share", "free_share", "total_mv", "circ_mv",
			},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"adj_factor": {
			Name:         "adj_factor",
			Dataset:      "adj_factor",
			BuildFilter:  codeDateFilter,
			SelectFields: []string{"ts_code", "trade_date", "adj_factor"},
			// 默认只取后复权因子(tushare adj_factor 语义);可传 factor_type=qfq 取前复权。
			FixedPartitions: map[string]string{"factor_type": "hfq"},
			PartitionParams: map[string]string{"factor_type": "factor_type"},
			DefaultLimit:    6000,
			MaxLimit:        10000,
		},
		"stk_mins": {
			Name:         "stk_mins",
			Dataset:      "bars_1m",
			BuildFilter:  minsFilter,
			SelectFields: []string{"ts_code", "trade_time", "open", "high", "low", "close", "vol", "amount"},
			// tushare stk_mins:vol 单位为股、amount 单位为元;内部存储为 手/千元。
			Transforms: []Transform{
				{Field: "vol", Scale: 100},
				{Field: "amount", Scale: 1000},
			},
			DefaultLimit: 8000,
			MaxLimit:     20000,
		},
		"stock_basic": {
			Name:         "stock_basic",
			Dataset:      "stock_basic",
			BuildFilter:  basicFilter,
			SelectFields: []string{"ts_code", "symbol", "name", "area", "industry", "market", "list_status", "list_date", "delist_date", "is_hs", "exchange", "fullname", "enname", "cnspell", "curr_type", "act_name", "act_ent_type"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"trade_cal": {
			Name:         "trade_cal",
			Dataset:      "trade_cal",
			BuildFilter:  calendarFilter,
			SelectFields: []string{"exchange", "cal_date", "is_open", "pretrade_date"},
			DefaultLimit: 10000,
			MaxLimit:     20000,
		},
		"stk_limit": {
			Name:         "stk_limit",
			Dataset:      "stk_limit",
			BuildFilter:  codeDateFilter,
			SelectFields: []string{"ts_code", "trade_date", "pre_close", "up_limit", "down_limit"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"suspend_d": {
			Name:         "suspend_d",
			Dataset:      "suspend_d",
			BuildFilter:  codeDateFilter,
			SelectFields: []string{"ts_code", "trade_date", "suspend_timing", "suspend_type"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"namechange": {
			Name:         "namechange",
			Dataset:      "namechange",
			BuildFilter:  namechangeFilter,
			SelectFields: []string{"ts_code", "name", "start_date", "end_date", "ann_date", "change_reason"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"index_daily": {
			Name:         "index_daily",
			Dataset:      "index_daily",
			BuildFilter:  codeDateFilter,
			SelectFields: []string{"ts_code", "trade_date", "close", "open", "high", "low", "pre_close", "change", "pct_chg", "vol", "amount"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
		"index_basic": {
			Name:         "index_basic",
			Dataset:      "index_basic",
			BuildFilter:  basicFilter,
			SelectFields: []string{"ts_code", "name", "fullname", "market", "publisher", "index_type", "category", "base_date", "base_point", "list_date"},
			DefaultLimit: 6000,
			MaxLimit:     10000,
		},
	}
}

// basicFilter 是 ts_code/market/list_status/name 的过滤(基础信息类接口)。
func basicFilter(ds *schema.Dataset, params Params) (*query.Filter, error) {
	var preds []query.Predicate
	for _, key := range []string{"ts_code", "market", "list_status", "name"} {
		v := params.Str(key)
		if v == "" {
			continue
		}
		idx, ok := ds.FieldIndex(key)
		if !ok {
			continue
		}
		preds = append(preds, query.In(idx, codeValues(v)...))
	}
	return &query.Filter{Preds: preds}, nil
}

// calendarFilter 是交易日历过滤:exchange + cal_date 区间。
func calendarFilter(ds *schema.Dataset, params Params) (*query.Filter, error) {
	var preds []query.Predicate
	if v := params.Str("exchange"); v != "" {
		idx, _ := ds.FieldIndex("exchange")
		preds = append(preds, query.In(idx, schema.Str(strings.ToUpper(v))))
	}
	idx, _ := ds.FieldIndex("cal_date")
	if v := params.Str("start_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("start_date: %w", err)
		}
		preds = append(preds, query.Gte(idx, schema.Date(days)))
	}
	if v := params.Str("end_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("end_date: %w", err)
		}
		preds = append(preds, query.Lte(idx, schema.Date(days)))
	}
	return &query.Filter{Preds: preds}, nil
}

// namechangeFilter 是名称变更过滤:ts_code + start_date 区间。
func namechangeFilter(ds *schema.Dataset, params Params) (*query.Filter, error) {
	var preds []query.Predicate
	if v := params.Str("ts_code"); v != "" {
		idx, _ := ds.FieldIndex("ts_code")
		preds = append(preds, query.In(idx, codeValues(v)...))
	}
	idx, _ := ds.FieldIndex("start_date")
	if v := params.Str("start_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("start_date: %w", err)
		}
		preds = append(preds, query.Gte(idx, schema.Date(days)))
	}
	if v := params.Str("end_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("end_date: %w", err)
		}
		preds = append(preds, query.Lte(idx, schema.Date(days)))
	}
	return &query.Filter{Preds: preds}, nil
}

// codeDateFilter 是 ts_code + trade_date/start_date/end_date 的通用过滤器。
func codeDateFilter(ds *schema.Dataset, params Params) (*query.Filter, error) {
	var preds []query.Predicate
	if v := params.Str("ts_code"); v != "" {
		idx, ok := ds.FieldIndex("ts_code")
		if !ok {
			return nil, fmt.Errorf("dataset %s has no ts_code field", ds.Name)
		}
		preds = append(preds, query.In(idx, codeValues(v)...))
	}
	dateIdx, ok := ds.FieldIndex("trade_date")
	if !ok {
		return nil, fmt.Errorf("dataset %s has no trade_date field", ds.Name)
	}
	if v := params.Str("trade_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("trade_date: %w", err)
		}
		preds = append(preds, query.Eq(dateIdx, schema.Date(days)))
	}
	if v := params.Str("start_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("start_date: %w", err)
		}
		preds = append(preds, query.Gte(dateIdx, schema.Date(days)))
	}
	if v := params.Str("end_date"); v != "" {
		days, err := schema.ParseDate(v)
		if err != nil {
			return nil, fmt.Errorf("end_date: %w", err)
		}
		preds = append(preds, query.Lte(dateIdx, schema.Date(days)))
	}
	return &query.Filter{Preds: preds}, nil
}

// minsFilter 是 stk_mins 的过滤器:ts_code + trade_time 区间。
func minsFilter(ds *schema.Dataset, params Params) (*query.Filter, error) {
	if freq := params.Str("freq"); freq != "" && freq != "1min" {
		return nil, fmt.Errorf("freq=%s is not supported yet (only 1min)", freq)
	}
	var preds []query.Predicate
	if v := params.Str("ts_code"); v != "" {
		idx, ok := ds.FieldIndex("ts_code")
		if !ok {
			return nil, fmt.Errorf("dataset %s has no ts_code field", ds.Name)
		}
		preds = append(preds, query.In(idx, codeValues(v)...))
	}
	tsIdx, ok := ds.FieldIndex("trade_time")
	if !ok {
		return nil, fmt.Errorf("dataset %s has no trade_time field", ds.Name)
	}
	if v := params.Str("start_date"); v != "" {
		micros, err := parseDateTimeParam(v)
		if err != nil {
			return nil, fmt.Errorf("start_date: %w", err)
		}
		preds = append(preds, query.Gte(tsIdx, schema.Timestamp(micros)))
	}
	if v := params.Str("end_date"); v != "" {
		micros, err := parseDateTimeParam(v)
		if err != nil {
			return nil, fmt.Errorf("end_date: %w", err)
		}
		preds = append(preds, query.Lte(tsIdx, schema.Timestamp(micros)))
	}
	return &query.Filter{Preds: preds}, nil
}

// codeValues 把逗号分隔的代码串转换为值列表。
func codeValues(s string) []schema.Value {
	parts := strings.Split(s, ",")
	out := make([]schema.Value, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, schema.Str(p))
	}
	return out
}

// parseDateTimeParam 解析常见日期时间格式为 micros,兼容 tushare 各接口的写法:
// "2006-01-02 15:04:05" / "20060102 15:04:05" / "20060102150405" / "20060102" 等。
func parseDateTimeParam(s string) (int64, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"20060102 15:04:05",
		"20060102 15:04",
		"20060102150405",
		"2006-01-02",
		"20060102",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMicro(), nil
		}
	}
	return 0, fmt.Errorf("invalid datetime %q", s)
}
