package ingest

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
	"quant-core/internal/source"
)

// Importer 用任意数据源插件(source.Source)把数据集写入数据湖。
//
// 与具体数据源解耦:调用方传入 source.Source,数据源可以是
// tushare 官方、中转站插件,也可以是 exec 外部命令插件。
type Importer struct {
	Source source.Source
	Lake   *lake.Lake
	Logger *lake.BatchLogger
	Logf   func(format string, args ...any)
	// WindowRetries 是单个查询窗口在失败后的重试次数(默认 3)。
	// 中转站的上游池耗时常为瞬时错误,窗口级重试可显著提高批量导入成功率。
	WindowRetries int
	// RetryBackoff 是窗口重试的基础退避时长(默认 2s,线性递增)。
	RetryBackoff time.Duration
	// Resume 为 true 时,从 manifest 读取已完成窗口并跳过(重跑不重复写入)。
	Resume bool
}

// doneWindows 从 manifest 提取某数据集已完成的时间窗口。
// 窗口标签由写入记录形如 "<source>:<api>:<windowStart>" 的 Source 字段提供。
func (t *Importer) doneWindows(ds *schema.Dataset, apiName string) map[string]bool {
	if !t.Resume || t.Logger == nil {
		return nil
	}
	batches, err := t.Logger.Read(ds.Name)
	if err != nil || len(batches) == 0 {
		return nil
	}
	marker := ":" + apiName + ":"
	out := map[string]bool{}
	for _, b := range batches {
		if idx := strings.LastIndex(b.Source, marker); idx >= 0 {
			out[b.Source[idx+len(marker):]] = true
		}
	}
	return out
}

func (t *Importer) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// ImportMode 决定导入策略。
type ImportMode string

const (
	// ModeSnapshot 全量快照:按切片参数(或单次)拉取,写入无年份分区。
	ModeSnapshot ImportMode = "snapshot"
	// ModeDateRange 按日期区间逐日拉取,追加写入年份分区。
	ModeDateRange ImportMode = "date-range"
	// ModeMonthRange 按月区间拉取(单次查询覆盖整月),按行日期写入年份分区。
	// 适合支持 start_date/end_date 的接口,API 调用量约为逐日模式的 1/20。
	ModeMonthRange ImportMode = "month-range"
)

// Spec 描述一个数据集如何从数据源拉取。
type Spec struct {
	Dataset string
	APIName string
	Mode    ImportMode
	// Slices 是快照模式的分片参数(每片一次请求);为空时单次全量拉取。
	// 例:stock_basic 按 list_status 分三片。
	Slices []map[string]string
	// StaticParams 是每次请求都携带的固定参数。
	StaticParams map[string]string
	// PartitionFromField 指定快照模式的分区值取自哪个字段(如 list_status)。
	PartitionFromField string
	// DateParam 是逐日模式下按日迭代时使用的参数名(trade_date)。
	DateParam string
	// DateField 是行内用于确定分区年份的日期字段(month-range 模式使用)。
	DateField string
	// WindowDays 是区间模式的窗口天数(默认 31,即按月);≤366 以适配中转站限制。
	WindowDays int
	// Fields 是请求的列裁剪(空串表示全列)。
	Fields string
}

// Specs 返回内置的 tushare 数据集规格(源无关)。
func Specs() []*Spec {
	return []*Spec{
		{
			Dataset: "stock_basic", APIName: "stock_basic", Mode: ModeSnapshot,
			// 细化切片:list_status × exchange,保证任一中转站的单次上限内(A 站硬上限 5000)
			Slices:             stockBasicSlices(),
			PartitionFromField: "list_status",
			Fields:             "ts_code,symbol,name,area,industry,fullname,enname,cnspell,market,exchange,curr_type,list_status,list_date,delist_date,is_hs,act_name,act_ent_type",
		},
		{
			Dataset: "trade_cal", APIName: "trade_cal", Mode: ModeSnapshot,
			// 中转站限制单次日期窗口 ≤ 366 天,交易日历必须按 交易所 × 年份 切片
			Slices:             tradeCalSlices(),
			PartitionFromField: "exchange",
			Fields:             "exchange,cal_date,is_open,pretrade_date",
		},
		{
			Dataset: "index_basic", APIName: "index_basic", Mode: ModeSnapshot,
			Slices: []map[string]string{
				{"market": "SSE"},
				{"market": "SZSE"},
				{"market": "CSI"},
				{"market": "CNI"},
				{"market": "SW"},
			},
			PartitionFromField: "market",
			Fields:             "ts_code,name,fullname,market,publisher,index_type,category,base_date,base_point,list_date",
		},
		{
			Dataset: "stk_limit", APIName: "stk_limit", Mode: ModeDateRange,
			DateParam: "trade_date",
			Fields:    "ts_code,trade_date,pre_close,up_limit,down_limit",
		},
		{
			Dataset: "suspend_d", APIName: "suspend_d", Mode: ModeMonthRange,
			DateField: "trade_date",
			Fields:    "ts_code,trade_date,suspend_timing,suspend_type",
		},
		{
			Dataset: "namechange", APIName: "namechange", Mode: ModeMonthRange,
			DateField: "start_date",
			Fields:    "ts_code,name,start_date,end_date,ann_date,change_reason",
		},
		{
			Dataset: "index_daily", APIName: "index_daily", Mode: ModeMonthRange,
			DateField:  "trade_date",
			WindowDays: 366, // 按年窗口,避免中转站单次行数上限
			Slices:     indexDailySlices(),
			Fields:     "ts_code,trade_date,close,open,high,low,pre_close,change,pct_chg,vol,amount",
		},
	}
}

// SpecByName 按数据集名查找规格。
func SpecByName(dataset string) (*Spec, error) {
	for _, spec := range Specs() {
		if spec.Dataset == dataset {
			return spec, nil
		}
	}
	return nil, fmt.Errorf("未知数据集 %q(可用: %s)", dataset, strings.Join(SpecNames(), ", "))
}

// stockBasicSlices 生成股票列表切片:list_status × exchange。
func stockBasicSlices() []map[string]string {
	statuses := []string{"L", "D", "P"}
	exchanges := []string{"SSE", "SZSE", "BSE"}
	out := make([]map[string]string, 0, len(statuses)*len(exchanges))
	for _, status := range statuses {
		for _, exchange := range exchanges {
			out = append(out, map[string]string{"list_status": status, "exchange": exchange})
		}
	}
	return out
}

// tradeCalSlices 生成交易日历切片:交易所 × 年份(中转站单窗口 ≤ 366 天)。
func tradeCalSlices() []map[string]string {
	exchanges := []string{"SSE", "SZSE", "CFFEX"}
	firstYear, lastYear := 1990, time.Now().Year()
	out := make([]map[string]string, 0, len(exchanges)*(lastYear-firstYear+1))
	for _, exchange := range exchanges {
		for year := firstYear; year <= lastYear; year++ {
			out = append(out, map[string]string{
				"exchange":   exchange,
				"start_date": fmt.Sprintf("%d0101", year),
				"end_date":   fmt.Sprintf("%d1231", year),
			})
		}
	}
	return out
}

// indexDailySlices 生成指数日线的代码切片(市场主要宽基指数)。
// 指数总数上万,不按代码切片会触发单次行数上限与不必要的数据量。
func indexDailySlices() []map[string]string {
	codes := []string{
		"000001.SH", // 上证指数
		"000016.SH", // 上证50
		"000300.SH", // 沪深300
		"000905.SH", // 中证500
		"000852.SH", // 中证1000
		"000688.SH", // 科创50
		"399001.SZ", // 深证成指
		"399006.SZ", // 创业板指
		"399303.SZ", // 国证2000
		"899050.BJ", // 北证50
	}
	out := make([]map[string]string, 0, len(codes))
	for _, code := range codes {
		out = append(out, map[string]string{"ts_code": code})
	}
	return out
}

// SpecNames 返回全部规格名。
func SpecNames() []string {
	out := make([]string, 0, len(Specs()))
	for _, spec := range Specs() {
		out = append(out, spec.Dataset)
	}
	return out
}

// ImportOptions 控制导入范围。
type ImportOptions struct {
	StartDate time.Time // 区间模式起点(含)
	EndDate   time.Time // 区间模式终点(含)
	// Replace 为 true 时,快照模式会先清空目标分区再写入。
	Replace bool
}

// Import 执行导入。
func (t *Importer) Import(ctx context.Context, spec *Spec, opts ImportOptions) (int64, error) {
	ds, err := t.Lake.Registry.Get(spec.Dataset)
	if err != nil {
		return 0, err
	}
	switch spec.Mode {
	case ModeSnapshot:
		return t.importSnapshot(ctx, ds, spec, opts)
	case ModeDateRange:
		return t.importDateRange(ctx, ds, spec, opts)
	case ModeMonthRange:
		return t.importMonthRange(ctx, ds, spec, opts)
	default:
		return 0, fmt.Errorf("dataset %s: 不支持的导入模式 %q", spec.Dataset, spec.Mode)
	}
}

// fetch 调用数据源并做完整性检查;瞬时失败按窗口级重试。
func (t *Importer) fetch(ctx context.Context, spec *Spec, params map[string]any) (*source.Result, error) {
	merged := map[string]any{}
	for k, v := range spec.StaticParams {
		merged[k] = v
	}
	for k, v := range params {
		merged[k] = v
	}
	retries := t.WindowRetries
	if retries <= 0 {
		retries = 3
	}
	backoff := t.RetryBackoff
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			t.logf("dataset %s 窗口重试 %d/%d(%v)", spec.Dataset, attempt, retries, lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff * time.Duration(attempt)):
			}
		}
		result, err := t.Source.Call(ctx, spec.APIName, merged, spec.Fields)
		if err != nil {
			lastErr = fmt.Errorf("dataset %s(%s): %w", spec.Dataset, t.Source.Name(), err)
			continue
		}
		if err := checkComplete(spec, result); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, lastErr
}

// checkComplete 做截断检测。
//
// 中转站的 limit 语义不完全一致(有的忽略小 limit 返回全量,有的按 limit 截断
// 且把 count 一起改小),因此采用两种信号:
//  1. count > 返回行数:确定的截断;
//  2. 返回行数恰好等于本次 limit:可疑截断(避免误报"返回多于 limit"的情形)。
func checkComplete(spec *Spec, result *source.Result) error {
	if result.Count > 0 && int64(len(result.Items)) < result.Count {
		return fmt.Errorf("dataset %s: 结果被截断(返回 %d / 共 %d),请缩小查询窗口或提高 limit",
			spec.Dataset, len(result.Items), result.Count)
	}
	if result.Limit > 0 && len(result.Items) == result.Limit {
		return fmt.Errorf("dataset %s: 返回行数 %d 恰好等于单次上限 limit=%d,可能被截断,请缩小查询窗口",
			spec.Dataset, len(result.Items), result.Limit)
	}
	return nil
}

// importSnapshot 拉取快照数据并按分区字段写入。
// 先收集全部切片的行,再按分区写入一次(Replace 时每个分区只清空一次,
// 避免多切片互相覆盖)。
func (t *Importer) importSnapshot(ctx context.Context, ds *schema.Dataset, spec *Spec, opts ImportOptions) (int64, error) {
	slices := spec.Slices
	if len(slices) == 0 {
		slices = []map[string]string{nil}
	}
	// 收集:分区键 → 行
	groups := map[string][][]schema.Value{}
	order := []string{}
	var total int64
	for sliceIdx, slice := range slices {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		params := map[string]any{}
		for k, v := range slice {
			params[k] = v
		}
		result, err := t.fetch(ctx, spec, params)
		if err != nil {
			return total, err
		}
		if len(result.Items) == 0 {
			t.logf("imported %s slice=%v rows=0", ds.Name, slice)
			continue
		}
		for _, item := range result.Items {
			row, err := t.rowFromItem(ds, result.Fields, item)
			if err != nil {
				return total, err
			}
			key := ""
			if len(ds.Partitions) > 0 && spec.PartitionFromField != "" {
				if idx := indexOf(result.Fields, spec.PartitionFromField); idx >= 0 && idx < len(item) {
					if s, ok := item[idx].(string); ok && s != "" {
						key = s
					}
				}
			}
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], row)
		}
		t.logf("fetched %s slice=%d/%d rows=%d", ds.Name, sliceIdx+1, len(slices), len(result.Items))
	}
	sort.Strings(order)
	for _, key := range order {
		rows := groups[key]
		values := map[string]string{}
		if len(ds.Partitions) > 0 {
			values[ds.Partitions[0]] = key
		}
		rel, err := lake.PartitionPath(ds, values)
		if err != nil {
			return total, err
		}
		dir := t.Lake.Dir(ds)
		if rel != "" {
			dir = filepath.Join(dir, rel)
		}
		if opts.Replace {
			if err := lake.RemovePartitionFiles(dir); err != nil {
				return total, err
			}
		}
		pw, err := lake.NewPartWriter(ds, dir)
		if err != nil {
			return total, err
		}
		for _, row := range rows {
			if err := pw.WriteRow(row); err != nil {
				_, _ = pw.Close()
				return total, err
			}
		}
		files, err := pw.Close()
		if err != nil {
			return total, err
		}
		total += int64(len(rows))
		if t.Logger != nil {
			for i := range files {
				if rel != "" {
					files[i].RelPath = rel + "/" + files[i].RelPath
				}
			}
			partitions := []string{}
			if len(ds.Partitions) > 0 {
				partitions = []string{ds.Partitions[0] + "=" + key}
			}
			if err := t.Logger.Append(lake.Batch{
				ID: batchID(ds.Name, 0), Dataset: ds.Name, Operation: "import",
				Source: t.Source.Name() + ":" + spec.APIName, CreatedAt: nowUTC(), Rows: int64(len(rows)),
				Partitions: partitions, Files: files,
			}); err != nil {
				return total, err
			}
		}
		t.logf("imported %s source=%s partition=%q rows=%d", ds.Name, t.Source.Name(), key, len(rows))
	}
	return total, nil
}

// importDateRange 按日期迭代拉取并写入。
func (t *Importer) importDateRange(ctx context.Context, ds *schema.Dataset, spec *Spec, opts ImportOptions) (int64, error) {
	start, end := opts.StartDate, opts.EndDate
	if start.IsZero() || end.IsZero() {
		return 0, fmt.Errorf("dataset %s: %s 模式需要 start_date 与 end_date", ds.Name, spec.Mode)
	}
	var total int64
	done := t.doneWindows(ds, spec.APIName)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		dateStr := day.Format("20060102")
		if done[dateStr] {
			continue
		}
		result, err := t.fetch(ctx, spec, map[string]any{spec.DateParam: dateStr})
		if err != nil {
			return total, err
		}
		if len(result.Items) == 0 {
			continue
		}
		year := day.Format("2006")
		wrote, err := t.writeRows(ds, spec, result, map[string]string{"year": year}, dateStr)
		if err != nil {
			return total, err
		}
		total += wrote
	}
	return total, nil
}

// importMonthRange 按固定窗口(默认按月)拉取,并按行自身的日期字段写入年份分区。
// 支持 Slices(如按指数代码分片)与 WindowDays(如按年窗口以适配中转站限制)。
func (t *Importer) importMonthRange(ctx context.Context, ds *schema.Dataset, spec *Spec, opts ImportOptions) (int64, error) {
	start, end := opts.StartDate, opts.EndDate
	if start.IsZero() || end.IsZero() {
		return 0, fmt.Errorf("dataset %s: %s 模式需要 start_date 与 end_date", ds.Name, spec.Mode)
	}
	if spec.DateField == "" {
		return 0, fmt.Errorf("dataset %s: %s 模式需要 date_field", ds.Name, spec.Mode)
	}
	dateIdx, ok := ds.FieldIndex(spec.DateField)
	if !ok {
		return 0, fmt.Errorf("dataset %s: 日期字段 %q 不在 schema 中", ds.Name, spec.DateField)
	}
	windowDays := spec.WindowDays
	if windowDays <= 0 {
		windowDays = 31
	}
	slices := spec.Slices
	if len(slices) == 0 {
		slices = []map[string]string{nil}
	}

	var total int64
	done := t.doneWindows(ds, spec.APIName)
	for sliceIdx, slice := range slices {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		for windowStart := start; !windowStart.After(end); windowStart = windowStart.AddDate(0, 0, windowDays) {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			if done[windowStart.Format("20060102")] {
				continue
			}
			windowEnd := windowStart.AddDate(0, 0, windowDays-1)
			if windowEnd.After(end) {
				windowEnd = end
			}
			params := map[string]any{
				"start_date": windowStart.Format("20060102"),
				"end_date":   windowEnd.Format("20060102"),
			}
			for k, v := range slice {
				params[k] = v
			}
			result, err := t.fetch(ctx, spec, params)
			if err != nil {
				return total, err
			}
			if len(result.Items) == 0 {
				continue
			}
			byYear := map[string][][]schema.Value{}
			for _, item := range result.Items {
				row, err := t.rowFromItem(ds, result.Fields, item)
				if err != nil {
					return total, err
				}
				year := "unknown"
				if !row[dateIdx].IsNull() {
					year = schema.TimeFromDays(row[dateIdx].I).Format("2006")
				}
				byYear[year] = append(byYear[year], row)
			}
			years := make([]string, 0, len(byYear))
			for y := range byYear {
				years = append(years, y)
			}
			sort.Strings(years)
			for _, year := range years {
				rows := byYear[year]
				rel, err := lake.PartitionPath(ds, map[string]string{"year": year})
				if err != nil {
					return total, err
				}
				dir := t.Lake.Dir(ds)
				if rel != "" {
					dir = filepath.Join(dir, rel)
				}
				pw, err := lake.NewPartWriter(ds, dir)
				if err != nil {
					return total, err
				}
				for _, row := range rows {
					if err := pw.WriteRow(row); err != nil {
						_, _ = pw.Close()
						return total, err
					}
				}
				files, err := pw.Close()
				if err != nil {
					return total, err
				}
				total += int64(len(rows))
				if t.Logger != nil {
					for i := range files {
						if rel != "" {
							files[i].RelPath = rel + "/" + files[i].RelPath
						}
					}
					if err := t.Logger.Append(lake.Batch{
						ID: batchID(ds.Name, sliceIdx), Dataset: ds.Name, Operation: "import",
						Source: t.Source.Name() + ":" + spec.APIName + ":" + windowStart.Format("20060102"),
						CreatedAt: nowUTC(), Rows: int64(len(rows)),
						Partitions: []string{"year=" + year}, Files: files,
					}); err != nil {
						return total, err
					}
				}
			}
			t.logf("imported %s slice=%d/%d %s~%s rows=%d", ds.Name, sliceIdx+1, len(slices),
				windowStart.Format("20060102"), windowEnd.Format("20060102"), len(result.Items))
		}
	}
	return total, nil
}

// writeRows 把一次结果整体写入某个分区。
func (t *Importer) writeRows(ds *schema.Dataset, spec *Spec, result *source.Result, partValues map[string]string, label string) (int64, error) {
	rel, err := lake.PartitionPath(ds, partValues)
	if err != nil {
		return 0, err
	}
	dir := t.Lake.Dir(ds)
	if rel != "" {
		dir = filepath.Join(dir, rel)
	}
	pw, err := lake.NewPartWriter(ds, dir)
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, item := range result.Items {
		row, err := t.rowFromItem(ds, result.Fields, item)
		if err != nil {
			_, _ = pw.Close()
			return wrote, err
		}
		if err := pw.WriteRow(row); err != nil {
			_, _ = pw.Close()
			return wrote, err
		}
		wrote++
	}
	files, err := pw.Close()
	if err != nil {
		return wrote, err
	}
	if t.Logger != nil {
		for i := range files {
			if rel != "" {
				files[i].RelPath = rel + "/" + files[i].RelPath
			}
		}
		partitions := make([]string, 0, len(partValues))
		for k, v := range partValues {
			partitions = append(partitions, k+"="+v)
		}
		sort.Strings(partitions)
		if err := t.Logger.Append(lake.Batch{
			ID: batchID(ds.Name, 0), Dataset: ds.Name, Operation: "import",
			Source: t.Source.Name() + ":" + spec.APIName + ":" + label,
			CreatedAt: nowUTC(), Rows: wrote, Partitions: partitions, Files: files,
		}); err != nil {
			return wrote, err
		}
	}
	return wrote, nil
}

// rowFromItem 按字段名把数据源返回项映射为逻辑值行。
func (t *Importer) rowFromItem(ds *schema.Dataset, fields []string, item []any) ([]schema.Value, error) {
	row := make([]schema.Value, len(ds.Fields))
	for i := range row {
		row[i] = schema.NullValue()
	}
	for i, name := range fields {
		if i >= len(item) {
			break
		}
		di, ok := ds.FieldIndex(name)
		if !ok {
			continue
		}
		v, err := convertSourceValue(item[i], ds.Fields[di].Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}
		row[di] = v
	}
	return row, nil
}

// convertSourceValue 把数据源的 JSON 值转换为逻辑值。
func convertSourceValue(raw any, ft schema.FieldType) (schema.Value, error) {
	if raw == nil {
		return schema.NullValue(), nil
	}
	switch ft {
	case schema.TypeString:
		switch v := raw.(type) {
		case string:
			return schema.Str(v), nil
		case float64:
			return schema.Str(fmt.Sprintf("%v", v)), nil
		default:
			return schema.NullValue(), fmt.Errorf("cannot convert %T to string", raw)
		}
	case schema.TypeDate:
		s, ok := raw.(string)
		if !ok || s == "" {
			return schema.NullValue(), nil
		}
		days, err := schema.ParseDate(s)
		if err != nil {
			return schema.NullValue(), err
		}
		return schema.Date(days), nil
	case schema.TypeTimestamp:
		s, ok := raw.(string)
		if !ok || s == "" {
			return schema.NullValue(), nil
		}
		tm, err := parseAnyTime(s)
		if err != nil {
			return schema.NullValue(), err
		}
		return schema.Timestamp(schema.MicrosFromTime(tm)), nil
	case schema.TypeFloat64:
		f, ok := raw.(float64)
		if !ok {
			if s, isStr := raw.(string); isStr {
				if s == "" {
					return schema.NullValue(), nil
				}
				return schema.NullValue(), fmt.Errorf("cannot convert %q to float", s)
			}
			return schema.NullValue(), fmt.Errorf("cannot convert %T to float", raw)
		}
		return schema.Float(f), nil
	case schema.TypeInt64:
		switch v := raw.(type) {
		case float64:
			return schema.Int(int64(v)), nil
		case string:
			if v == "" {
				return schema.NullValue(), nil
			}
			return schema.NullValue(), fmt.Errorf("cannot convert %q to int", v)
		default:
			return schema.NullValue(), fmt.Errorf("cannot convert %T to int", raw)
		}
	case schema.TypeBool:
		if b, ok := raw.(bool); ok {
			return schema.Bool(b), nil
		}
		return schema.NullValue(), fmt.Errorf("cannot convert %T to bool", raw)
	default:
		return schema.NullValue(), fmt.Errorf("unsupported type %q", ft)
	}
}

func parseAnyTime(s string) (time.Time, error) {
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02",
		"20060102",
		"20060102150405",
	}
	for _, layout := range layouts {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid datetime %q", s)
}

func indexOf(list []string, target string) int {
	for i, s := range list {
		if s == target {
			return i
		}
	}
	return -1
}
