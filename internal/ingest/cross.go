package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// BuildCrossOptions 控制截面物化。
type BuildCrossOptions struct {
	// Year 非空时只物化该年份(YYYY)。
	Year string
	// Source 是源数据集(默认 bars_daily),Target 是目标数据集(默认 daily_cross)。
	Source string
	Target string
}

// BuildCross 把按 (ts_code, trade_date) 排序的日线表物化为按 (trade_date, ts_code)
// 排序的横截面表:全市场单日查询从"扫半个年文件"降为"读一个行组"。
//
// 内存按天分块(一天最多约 6 千行),与年份文件大小无关。
func (t *Importer) BuildCross(opts BuildCrossOptions) (int64, error) {
	srcName, dstName := opts.Source, opts.Target
	if srcName == "" {
		srcName = "bars_daily"
	}
	if dstName == "" {
		dstName = "daily_cross"
	}
	src, err := t.Lake.Registry.Get(srcName)
	if err != nil {
		return 0, err
	}
	dst, err := t.Lake.Registry.Get(dstName)
	if err != nil {
		return 0, err
	}
	// 源列 = 目标字段名(顺序与目标一致,写回时可直接按位拷贝)
	fields := make([]string, 0, len(dst.Fields))
	for _, f := range dst.Fields {
		if _, ok := src.FieldIndex(f.Name); !ok {
			return 0, fmt.Errorf("源数据集 %s 缺少字段 %s", srcName, f.Name)
		}
		fields = append(fields, f.Name)
	}

	years, err := t.yearsOf(src)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, year := range years {
		if opts.Year != "" && year != opts.Year {
			continue
		}
		// 物化语义:按年整体替换(重跑不追加重复行)
		rel, err := lake.PartitionPath(dst, map[string]string{"year": year})
		if err != nil {
			return total, err
		}
		if err := os.RemoveAll(filepath.Join(t.Lake.Dir(dst), rel)); err != nil {
			return total, err
		}
		days, err := t.readYearByDay(src, fields, year)
		if err != nil {
			return total, err
		}
		written, err := t.writeCrossYear(dst, days, year)
		if err != nil {
			return total, err
		}
		total += written
		t.logf("daily_cross %s: %d rows", year, written)
	}
	return total, nil
}

// yearsOf 返回源数据集 year 分区下的年份列表。
func (t *Importer) yearsOf(ds *schema.Dataset) ([]string, error) {
	files, err := t.Lake.WalkFiles(ds, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, f := range files {
		year := f.Partitions["year"]
		if year == "" || seen[year] {
			continue
		}
		seen[year] = true
		out = append(out, year)
	}
	sort.Strings(out)
	return out, nil
}

// readYearByDay 读取某年源数据并按天分块返回(每天一批,内存有界)。
func (t *Importer) readYearByDay(ds *schema.Dataset, fields []string, year string) ([][]([]schema.Value), error) {
	scanner := query.NewCachedScanner(t.Lake, 64)
	cur, err := scanner.Open(query.Request{
		Dataset:    ds.Name,
		Columns:    fields,
		Partitions: map[string][]string{"year": {year}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close()
	dayIdx, _ := ds.FieldIndex("trade_date")
	codeIdx, _ := ds.FieldIndex("ts_code")
	byDay := map[int64][]([]schema.Value){}
	order := []int64{}
	for cur.Next() {
		row := cur.Row()
		day := row[dayIdx].I
		if _, ok := byDay[day]; !ok {
			order = append(order, day)
		}
		cp := make([]schema.Value, len(row))
		copy(cp, row)
		byDay[day] = append(byDay[day], cp)
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([][]([]schema.Value), 0, len(order))
	for _, day := range order {
		rows := byDay[day]
		sort.Slice(rows, func(i, j int) bool { return rows[i][codeIdx].S < rows[j][codeIdx].S })
		out = append(out, rows)
	}
	return out, nil
}

// writeCrossYear 把按天分块的源数据写入目标数据集(year 分区)。
func (t *Importer) writeCrossYear(dst *schema.Dataset, days [][]([]schema.Value), year string) (int64, error) {
	rel, err := lake.PartitionPath(dst, map[string]string{"year": year})
	if err != nil {
		return 0, err
	}
	pw, err := lake.NewPartWriter(dst, t.Lake.Dir(dst)+"/"+rel)
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, rows := range days {
		for _, src := range rows {
			if err := pw.WriteRow(src); err != nil {
				_, _ = pw.Close()
				return wrote, err
			}
			wrote++
		}
	}
	files, err := pw.Close()
	if err != nil {
		return wrote, err
	}
	if t.Logger != nil {
		relFiles := make([]lake.BatchFile, 0, len(files))
		for i := range files {
			files[i].RelPath = rel + "/" + files[i].RelPath
			relFiles = append(relFiles, files[i])
		}
		_ = t.Logger.Append(lake.Batch{
			ID:         fmt.Sprintf("materialize-daily_cross-%s-%d", year, time.Now().Unix()),
			Dataset:    dst.Name,
			Operation:  "materialize",
			Source:     "bars_daily->daily_cross:" + year,
			CreatedAt:  time.Now(),
			Rows:       wrote,
			Partitions: []string{"year=" + year},
			Files:      relFiles,
		})
	}
	return wrote, nil
}
