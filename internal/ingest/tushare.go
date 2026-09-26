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
	"quant-core/internal/source/tushare"
)

// TushareImporter 用 Tushare Pro 数据源写入数据湖。
type TushareImporter struct {
	Client *tushare.Client
	Lake   *lake.Lake
	Logger *lake.BatchLogger
	Logf   func(format string, args ...any)
}

func (t *TushareImporter) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// ImportMode 决定导入策略。
type ImportMode string

const (
	// ModeSnapshot 全量快照:拉取全部数据,按分区原子替换。
	ModeSnapshot ImportMode = "snapshot"
	// ModeDateRange 按日期区间拉取,逐日查询并追加写入年份分区。
	ModeDateRange ImportMode = "date-range"
	// ModeMonthRange 按月区间拉取(单次查询覆盖整月),按行的日期字段写入年份分区。
	// 适合支持 start_date/end_date 参数的接口,API 调用量约为逐日模式的 1/20。
	ModeMonthRange ImportMode = "month-range"
)

// TushareSpec 描述一个数据集如何从 tushare 拉取。
type TushareSpec struct {
	Dataset string
	APIName string
	Mode    ImportMode
	// StaticParams 是固定查询参数(如 exchange、market、list_status)。
	StaticParams map[string]string
	// PartitionFromField 指定分区值取自某个字段(如 list_status/market);空则由 StaticParams 提供。
	PartitionFromField string
	// DateParam 是区间模式下按日迭代时使用的参数名(trade_date)。
	DateParam string
	// DateField 是行内用于确定分区年份的日期字段(month-range 模式使用)。
	DateField string
}

// TushareSpecs 返回内置的 tushare 源定义。
func TushareSpecs() []*TushareSpec {
	return []*TushareSpec{
		{
			Dataset: "stock_basic", APIName: "stock_basic", Mode: ModeSnapshot,
			StaticParams:       map[string]string{"exchange": "", "list_status": ""},
			PartitionFromField: "list_status",
		},
		{
			Dataset: "trade_cal", APIName: "trade_cal", Mode: ModeSnapshot,
			StaticParams:       map[string]string{"exchange": "SSE"},
			PartitionFromField: "exchange",
		},
		{
			Dataset: "index_basic", APIName: "index_basic", Mode: ModeSnapshot,
			StaticParams:       map[string]string{"market": "SSE"},
			PartitionFromField: "market",
		},
		{
			Dataset: "stk_limit", APIName: "stk_limit", Mode: ModeMonthRange,
			DateField: "trade_date",
		},
		{
			Dataset: "suspend_d", APIName: "suspend_d", Mode: ModeMonthRange,
			DateField: "trade_date",
		},
		{
			Dataset: "namechange", APIName: "namechange", Mode: ModeMonthRange,
			DateField: "start_date",
		},
		{
			Dataset: "index_daily", APIName: "index_daily", Mode: ModeMonthRange,
			DateField: "trade_date",
		},
	}
}

// ImportOptions 控制导入范围。
type ImportOptions struct {
	StartDate time.Time // 区间模式起点(含)
	EndDate   time.Time // 区间模式终点(含)
	// Replace 为 true 时,快照模式会先清空目标分区再写入。
	Replace bool
}

// Import 执行导入。
func (t *TushareImporter) Import(ctx context.Context, spec *TushareSpec, opts ImportOptions) (int64, error) {
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
		return 0, fmt.Errorf("dataset %s: unsupported import mode %q", spec.Dataset, spec.Mode)
	}
}

// importSnapshot 拉取全量数据并按分区写入。
func (t *TushareImporter) importSnapshot(ctx context.Context, ds *schema.Dataset, spec *TushareSpec, opts ImportOptions) (int64, error) {
	params := map[string]any{}
	for k, v := range spec.StaticParams {
		params[k] = v
	}
	result, err := t.Client.CallAll(ctx, spec.APIName, params, t.fieldList(ds))
	if err != nil {
		return 0, err
	}
	groups := map[string][][]schema.Value{}
	order := []string{}
	for _, item := range result.Items {
		row, err := t.rowFromItem(ds, result.Fields, item)
		if err != nil {
			return 0, err
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
	sort.Strings(order)

	var total int64
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
				ID: batchID(ds.Name, len(order)), Dataset: ds.Name, Operation: "import",
				Source: "tushare:" + spec.APIName, CreatedAt: nowUTC(), Rows: int64(len(rows)),
				Partitions: partitions, Files: files,
			}); err != nil {
				return total, err
			}
		}
		t.logf("imported %s partition %q rows=%d", ds.Name, key, len(rows))
	}
	return total, nil
}

// importDateRange 按日期迭代拉取并写入。
func (t *TushareImporter) importDateRange(ctx context.Context, ds *schema.Dataset, spec *TushareSpec, opts ImportOptions) (int64, error) {
	start, end := opts.StartDate, opts.EndDate
	if start.IsZero() || end.IsZero() {
		return 0, fmt.Errorf("dataset %s: start_date and end_date are required for %s mode", ds.Name, spec.Mode)
	}
	var total int64
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		dateStr := day.Format("20060102")
		result, err := t.Client.CallAll(ctx, spec.APIName, map[string]any{spec.DateParam: dateStr}, t.fieldList(ds))
		if err != nil {
			return total, fmt.Errorf("dataset %s %s: %w", ds.Name, dateStr, err)
		}
		if len(result.Items) == 0 {
			continue
		}
		year := day.Format("2006")
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
		var wrote int64
		for _, item := range result.Items {
			row, err := t.rowFromItem(ds, result.Fields, item)
			if err != nil {
				_, _ = pw.Close()
				return total, err
			}
			if err := pw.WriteRow(row); err != nil {
				_, _ = pw.Close()
				return total, err
			}
			wrote++
		}
		files, err := pw.Close()
		if err != nil {
			return total, err
		}
		total += wrote
		if t.Logger != nil {
			for i := range files {
				files[i].RelPath = rel + "/" + files[i].RelPath
			}
			if err := t.Logger.Append(lake.Batch{
				ID: batchID(ds.Name, day.Day()), Dataset: ds.Name, Operation: "import",
				Source: "tushare:" + spec.APIName + ":" + dateStr, CreatedAt: nowUTC(),
				Rows: wrote, Partitions: []string{"year=" + year}, Files: files,
			}); err != nil {
				return total, err
			}
		}
		t.logf("imported %s %s rows=%d", ds.Name, dateStr, wrote)
	}
	return total, nil
}

// importMonthRange 按月区间拉取,并按行自身的日期字段写入对应年份分区。
func (t *TushareImporter) importMonthRange(ctx context.Context, ds *schema.Dataset, spec *TushareSpec, opts ImportOptions) (int64, error) {
	start, end := opts.StartDate, opts.EndDate
	if start.IsZero() || end.IsZero() {
		return 0, fmt.Errorf("dataset %s: start_date and end_date are required for %s mode", ds.Name, spec.Mode)
	}
	if spec.DateField == "" {
		return 0, fmt.Errorf("dataset %s: date_field is required for %s mode", ds.Name, spec.Mode)
	}
	dateIdx, ok := ds.FieldIndex(spec.DateField)
	if !ok {
		return 0, fmt.Errorf("dataset %s: date field %q not in schema", ds.Name, spec.DateField)
	}
	var total int64
	for cursor := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC); !cursor.After(end); cursor = cursor.AddDate(0, 1, 1) {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		monthStart := cursor
		if monthStart.Before(start) {
			monthStart = start
		}
		monthEnd := time.Date(cursor.Year(), cursor.Month()+1, 0, 0, 0, 0, 0, time.UTC)
		if monthEnd.After(end) {
			monthEnd = end
		}
		params := map[string]any{
			"start_date": monthStart.Format("20060102"),
			"end_date":   monthEnd.Format("20060102"),
		}
		for k, v := range spec.StaticParams {
			params[k] = v
		}
		result, err := t.Client.CallAll(ctx, spec.APIName, params, t.fieldList(ds))
		if err != nil {
			return total, fmt.Errorf("dataset %s %s: %w", ds.Name, monthStart.Format("200601"), err)
		}
		if len(result.Items) == 0 {
			t.logf("imported %s %s rows=0", ds.Name, monthStart.Format("200601"))
			continue
		}
		// 按行日期分组到年份分区
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
					ID: batchID(ds.Name, monthStart.Day()), Dataset: ds.Name, Operation: "import",
					Source: "tushare:" + spec.APIName + ":" + monthStart.Format("200601"),
					CreatedAt: nowUTC(), Rows: int64(len(rows)),
					Partitions: []string{"year=" + year}, Files: files,
				}); err != nil {
					return total, err
				}
			}
		}
		t.logf("imported %s %s rows=%d", ds.Name, monthStart.Format("200601"), len(result.Items))
	}
	return total, nil
}

// fieldList 返回数据集的字段名列表(逗号分隔),供 tushare 请求使用。
func (t *TushareImporter) fieldList(ds *schema.Dataset) string {
	return strings.Join(ds.FieldNames(), ",")
}

// rowFromItem 按字段名把 tushare 返回项映射为逻辑值行。
func (t *TushareImporter) rowFromItem(ds *schema.Dataset, fields []string, item []any) ([]schema.Value, error) {
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
		v, err := convertTushareValue(item[i], ds.Fields[di].Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}
		row[di] = v
	}
	return row, nil
}

// convertTushareValue 把 tushare 的 JSON 值转换为逻辑值。
func convertTushareValue(raw any, ft schema.FieldType) (schema.Value, error) {
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
