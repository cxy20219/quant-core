// lakediag 对数据湖做一次带统计的扫描,用于验证剪枝效果与查询行为。
//
// 用法:
//
//	go run ./tools/lakediag --lake D:\quant-lake --dataset bars_daily \
//	  --filter ts_code=600000.SH --start 20240101 --end 20241231
package main

import (
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

func main() {
	lakeDir := flag.String("lake", "lake", "数据湖根目录")
	registryPath := flag.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	dataset := flag.String("dataset", "bars_daily", "数据集名")
	filterSpec := flag.String("filter", "", "过滤条件,如 ts_code=600000.SH 或 ts_code=600000.SH,000001.SZ")
	start := flag.String("start", "", "起始日期 YYYYMMDD")
	end := flag.String("end", "", "结束日期 YYYYMMDD")
	debug := flag.Bool("debug", false, "输出行组计划")
	flag.Parse()

	reg, err := schema.Load(*registryPath)
	if err != nil {
		log.Fatalf("registry: %v", err)
	}
	ds, err := reg.Get(*dataset)
	if err != nil {
		log.Fatalf("dataset: %v", err)
	}
	l := lake.New(*lakeDir, reg)

	var preds []query.Predicate
	for _, part := range strings.Split(*filterSpec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			log.Fatalf("invalid filter %q (expect field=value)", part)
		}
		idx, ok := ds.FieldIndex(strings.TrimSpace(key))
		if !ok {
			log.Fatalf("dataset %s has no field %q", ds.Name, key)
		}
		ft := ds.Fields[idx].Type
		if ft == schema.TypeString {
			values := []schema.Value{}
			for _, v := range strings.Split(value, "|") {
				values = append(values, schema.Str(strings.TrimSpace(v)))
			}
			preds = append(preds, query.In(idx, values...))
			continue
		}
		days, err := schema.ParseDate(value)
		if err != nil {
			log.Fatalf("filter %s: %v", key, err)
		}
		if ft == schema.TypeTimestamp {
			preds = append(preds, query.In(idx, schema.Timestamp(days*86400*1_000_000)))
		} else {
			preds = append(preds, query.In(idx, schema.Date(days)))
		}
	}
	timeField := ds.TimeField()
	if timeField != "" {
		idx, _ := ds.FieldIndex(timeField)
		ft := ds.Fields[idx].Type
		if *start != "" {
			days, err := schema.ParseDate(*start)
			if err != nil {
				log.Fatalf("start: %v", err)
			}
			if ft == schema.TypeTimestamp {
				preds = append(preds, query.Gte(idx, schema.Timestamp(days*86400*1_000_000)))
			} else {
				preds = append(preds, query.Gte(idx, schema.Date(days)))
			}
		}
		if *end != "" {
			days, err := schema.ParseDate(*end)
			if err != nil {
				log.Fatalf("end: %v", err)
			}
			if ft == schema.TypeTimestamp {
				preds = append(preds, query.Lte(idx, schema.Timestamp((days+1)*86400*1_000_000-1)))
			} else {
				preds = append(preds, query.Lte(idx, schema.Date(days)))
			}
		}
	}

	var filter *query.Filter
	if len(preds) > 0 {
		filter = &query.Filter{Preds: preds}
	}

	sc := query.NewCachedScanner(l, 256)
	sc.Debug = *debug
	started := time.Now()
	cur, err := sc.Open(query.Request{Dataset: ds.Name, Filter: filter})
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer cur.Close()
	rows := 0
	for cur.Next() {
		rows++
	}
	if err := cur.Err(); err != nil {
		log.Fatalf("scan: %v", err)
	}
	stats := cur.Stats()
	fmt.Printf("dataset=%s rows=%d elapsed=%.1fms\n", ds.Name, rows, float64(time.Since(started).Microseconds())/1000)
	fmt.Printf("files_opened=%d files_skipped=%d meta_built=%d rowgroups_read=%d rowgroups_skipped=%d rows_seen=%d\n",
		stats.FilesOpened, stats.FilesSkipped, stats.MetaBuilt, stats.RowGroupsRead, stats.RowGroupsSkipped, stats.RowsSeen)
}
