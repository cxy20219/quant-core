package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// cmdDedupe 按注册表主键对数据集去重(逐分区重写)。
//
// 用途:导入任务中断后重跑可能重复写入同一窗口(断点续传实现前产生的数据);
// 该命令按 primary_key 保留首次出现的行,逐分区重写为单一 part 文件。
//
// 用法:
//
//	quantd dedupe --dataset namechange --lake <lake> [--apply]
//
// 默认只统计重复(--dry-run);加 --apply 才真正重写。
func cmdDedupe(args []string) {
	fs := flag.NewFlagSet("dedupe", flag.ExitOnError)
	lakeDir := fs.String("lake", "lake", "数据湖根目录")
	registryPath := fs.String("registry", filepath.Join("schemas", "datasets.yaml"), "数据集注册表")
	dataset := fs.String("dataset", "", "数据集名(必需)")
	maxRows := fs.Int("max-rows", 20_000_000, "单个分区的行数上限(超过则拒绝,避免内存风险)")
	apply := fs.Bool("apply", false, "真正执行重写(默认只统计)")
	reconcile := fs.Bool("reconcile", false, "只对账:把 manifest 与实际行数的差额记入 manifest(用于去重后修账)")
	_ = fs.Parse(args)

	if *dataset == "" {
		fs.Usage()
		os.Exit(2)
	}
	reg := loadRegistry(*registryPath)
	ds, err := reg.Get(*dataset)
	if err != nil {
		log.Fatal(err)
	}
	if *reconcile {
		reconcileLedger(lake.New(*lakeDir, reg), lake.NewBatchLogger(*lakeDir), ds)
		return
	}
	if len(ds.PrimaryKey) == 0 {
		log.Fatalf("数据集 %s 未定义 primary_key,无法去重", ds.Name)
	}
	keyIdx := make([]int, 0, len(ds.PrimaryKey))
	for _, name := range ds.PrimaryKey {
		idx, ok := ds.FieldIndex(name)
		if !ok {
			log.Fatalf("primary_key %q 不在字段中", name)
		}
		keyIdx = append(keyIdx, idx)
	}
	l := lake.New(*lakeDir, reg)
	logger := lake.NewBatchLogger(*lakeDir)
	refs, err := l.WalkFiles(ds, nil)
	if err != nil {
		log.Fatal(err)
	}
	// 收集全部分区组合
	combos := map[string]map[string]string{}
	for _, ref := range refs {
		key := partitionSignature(ds, ref.Partitions)
		combos[key] = ref.Partitions
	}
	signatures := make([]string, 0, len(combos))
	for sig := range combos {
		signatures = append(signatures, sig)
	}
	sort.Strings(signatures)

	var totalBefore, totalAfter int64
	for _, sig := range signatures {
		parts := combos[sig]
		dir, err := lake.PartitionDir(l, ds, parts)
		if err != nil {
			log.Fatal(err)
		}
		before, err := dirRowCount(dir)
		if err != nil {
			log.Fatalf("count %s: %v", dir, err)
		}
		if before > int64(*maxRows) {
			log.Fatalf("分区 %s 行数 %d 超过 --max-rows=%d,拒绝处理", sig, before, *maxRows)
		}
		rows, err := scanPartition(l, ds, parts)
		if err != nil {
			log.Fatalf("scan %s: %v", sig, err)
		}
		seen := make(map[string]bool, len(rows))
		kept := make([][]schema.Value, 0, len(rows))
		for _, row := range rows {
			key := primaryKeyString(row, keyIdx)
			if seen[key] {
				continue
			}
			seen[key] = true
			kept = append(kept, row)
		}
		dup := len(rows) - len(kept)
		totalBefore += int64(len(rows))
		totalAfter += int64(len(kept))
		if dup == 0 {
			fmt.Printf("[ok]   %-28s rows=%d 无重复\n", sig, len(rows))
			continue
		}
		if !*apply {
			fmt.Printf("[dup]  %-28s rows=%d 去重后=%d(重复 %d)\n", sig, len(rows), len(kept), dup)
			continue
		}
		if err := rewritePartition(l, ds, dir, kept); err != nil {
			log.Fatalf("rewrite %s: %v", sig, err)
		}
		fmt.Printf("[fix]  %-28s rows=%d -> %d(移除重复 %d)\n", sig, len(rows), len(kept), dup)
		// 记账:dedupe 以负数行数入 manifest,保证 verify 的账目平衡
		if logger != nil {
			if err := logger.Append(lake.Batch{
				ID:        fmt.Sprintf("dedupe-%s-%d", ds.Name, time.Now().UnixNano()),
				Dataset:   ds.Name,
				Operation: "dedupe",
				Source:    "dedupe",
				CreatedAt: time.Now().UTC(),
				Rows:      -int64(dup),
				Partitions: func() []string {
					if len(ds.Partitions) == 0 {
						return nil
					}
					return []string{sig}
				}(),
				Notes: fmt.Sprintf("去重移除 %d 行", dup),
			}); err != nil {
				log.Fatalf("write manifest: %v", err)
			}
		}
	}
	fmt.Printf("dataset=%s 总计: %d -> %d\n", ds.Name, totalBefore, totalAfter)
	if !*apply && totalBefore != totalAfter {
		fmt.Println("提示: 加 --apply 执行实际重写")
	}
}

// reconcileLedger 对账:把 manifest 合计与实际行数的差额写入一条 reconcile 记录。
func reconcileLedger(l *lake.Lake, logger *lake.BatchLogger, ds *schema.Dataset) {
	refs, err := l.WalkFiles(ds, nil)
	if err != nil {
		log.Fatal(err)
	}
	var actual int64
	for _, ref := range refs {
		n, err := dirRowCountForFile(ref.Path)
		if err != nil {
			log.Fatal(err)
		}
		actual += n
	}
	summary, err := logger.Summarize(ds.Name)
	if err != nil {
		log.Fatal(err)
	}
	diff := actual - summary.Rows
	fmt.Printf("dataset=%s manifest=%d actual=%d diff=%d\n", ds.Name, summary.Rows, actual, diff)
	if diff == 0 {
		fmt.Println("账目一致,无需修正")
		return
	}
	if err := logger.Append(lake.Batch{
		ID:        fmt.Sprintf("reconcile-%s-%d", ds.Name, time.Now().UnixNano()),
		Dataset:   ds.Name,
		Operation: "reconcile",
		Source:    "dedupe",
		CreatedAt: time.Now().UTC(),
		Rows:      diff,
		Notes:     "去重/清理后账目对账修正",
	}); err != nil {
		log.Fatalf("write manifest: %v", err)
	}
	fmt.Printf("已写入 reconcile 记录(rows=%d)\n", diff)
}

func dirRowCountForFile(path string) (int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return 0, err
	}
	pf, err := parquet.OpenFile(fh, st.Size())
	if err != nil {
		return 0, err
	}
	return pf.NumRows(), nil
}

func partitionSignature(ds *schema.Dataset, parts map[string]string) string {
	if len(ds.Partitions) == 0 {
		return "(no-partition)"
	}
	segs := make([]string, 0, len(ds.Partitions))
	for _, key := range ds.Partitions {
		segs = append(segs, key+"="+parts[key])
	}
	return strings.Join(segs, "/")
}



func dirRowCount(dir string) (int64, error) {
	files, err := lake.FilesInDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, path := range files {
		fh, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		st, err := fh.Stat()
		if err != nil {
			_ = fh.Close()
			return 0, err
		}
		pf, err := parquet.OpenFile(fh, st.Size())
		if err != nil {
			_ = fh.Close()
			return 0, err
		}
		total += pf.NumRows()
		if err := fh.Close(); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// scanPartition 读取一个分区的全部行。
func scanPartition(l *lake.Lake, ds *schema.Dataset, parts map[string]string) ([][]schema.Value, error) {
	explicit := map[string][]string{}
	for key, value := range parts {
		explicit[key] = []string{value}
	}
	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{Dataset: ds.Name, Columns: ds.FieldNames(), Partitions: explicit})
	if err != nil {
		return nil, err
	}
	defer cur.Close()
	var rows [][]schema.Value
	for cur.Next() {
		row := cur.Row()
		dup := make([]schema.Value, len(row))
		copy(dup, row)
		rows = append(rows, dup)
	}
	return rows, cur.Err()
}

// rewritePartition 用 kept 行替换分区内容(先写临时目录再原子替换)。
func rewritePartition(l *lake.Lake, ds *schema.Dataset, dir string, kept [][]schema.Value) error {
	tmpDir := dir + ".dedup-tmp"
	_ = os.RemoveAll(tmpDir)
	pw, err := lake.NewPartWriter(ds, tmpDir)
	if err != nil {
		return err
	}
	for _, row := range kept {
		if err := pw.WriteRow(row); err != nil {
			_, _ = pw.Close()
			return err
		}
	}
	files, err := pw.Close()
	if err != nil {
		return err
	}
	if len(files) != 1 {
		return fmt.Errorf("去重后应只有一个 part 文件,实际 %d", len(files))
	}
	oldFiles, err := lake.FilesInDir(dir)
	if err != nil {
		return err
	}
	backup := dir + fmt.Sprintf(".bak-%d", time.Now().Unix())
	if err := os.Rename(dir, backup); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	newPath := filepath.Join(tmpDir, files[0].RelPath)
	target := filepath.Join(dir, "part-0001.parquet")
	if err := os.Rename(newPath, target); err != nil {
		_ = os.Rename(backup, dir)
		return err
	}
	_ = os.RemoveAll(tmpDir)
	_ = os.RemoveAll(backup)
	_ = oldFiles
	return nil
}

func primaryKeyString(row []schema.Value, idx []int) string {
	var sb strings.Builder
	for i, j := range idx {
		if i > 0 {
			sb.WriteByte('|')
		}
		v := row[j]
		switch v.Kind {
		case schema.KindString:
			sb.WriteString(v.S)
		case schema.KindDate, schema.KindTimestamp, schema.KindInt:
			fmt.Fprintf(&sb, "%d", v.I)
		case schema.KindFloat:
			fmt.Fprintf(&sb, "%g", v.F)
		case schema.KindBool:
			fmt.Fprintf(&sb, "%v", v.B)
		default:
			sb.WriteString("~")
		}
	}
	return sb.String()
}
