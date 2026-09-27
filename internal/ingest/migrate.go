// Package ingest 负责把外部数据源导入数据湖。第一类源是旧版数据湖
// (quant-data 的 quant-store),它已按 code 排序,可流式转换。
package ingest

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/lake"
	"quant-core/internal/pq"
	"quant-core/internal/schema"
)

// SrcKind 是源列的逻辑类型。
type SrcKind uint8

const (
	SrcString SrcKind = iota
	SrcDate
	SrcTimestamp
	SrcFloat
)

// FieldMap 描述源列到目标字段的映射。Dst 为空表示丢弃该列。
type FieldMap struct {
	Src   string
	Dst   string
	Kind  SrcKind
	Scale float64 // 0/1 表示不缩放,否则乘以该系数
}

// Migration 描述一个数据集的迁移映射。
type Migration struct {
	Dataset string // 目标数据集名(注册表)
	SrcDir  string // 源湖中的子目录
	Fields  []FieldMap
}

// Migrator 执行旧湖到新湖的迁移。
type Migrator struct {
	SrcRoot string
	Lake    *lake.Lake
	Logger  *lake.BatchLogger
	Logf    func(format string, args ...any)
}

func (m *Migrator) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// MigrateOptions 控制迁移范围。
type MigrateOptions struct {
	// Year 非空时只迁移该年份的源文件(用于分批或试运行)。
	Year string
	// Jobs 是并行处理源文件的 worker 数(<=1 表示串行)。
	Jobs int
}

// Migrate 迁移一个数据集的全部(或指定年份)源文件。返回总行数。
func (m *Migrator) Migrate(mig *Migration, opts MigrateOptions) (int64, error) {
	ds, err := m.Lake.Registry.Get(mig.Dataset)
	if err != nil {
		return 0, err
	}
	srcRoot := filepath.Join(m.SrcRoot, filepath.FromSlash(mig.SrcDir))
	files, err := listParquet(srcRoot)
	if err != nil {
		return 0, err
	}
	var selected []string
	for _, file := range files {
		parts := parsePathPartitions(file)
		if opts.Year != "" && parts["year"] != "" && parts["year"] != opts.Year {
			continue
		}
		selected = append(selected, file)
	}

	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = 1
	}
	if jobs > len(selected) {
		jobs = len(selected)
	}
	if jobs <= 1 {
		return m.migrateSequential(ds, mig, selected)
	}
	return m.migrateParallel(ds, mig, selected, jobs)
}

func (m *Migrator) migrateSequential(ds *schema.Dataset, mig *Migration, files []string) (int64, error) {
	var total int64
	for i, file := range files {
		rows, err := m.migrateOne(ds, mig, file, i+1)
		if err != nil {
			return total, err
		}
		total += rows
	}
	return total, nil
}

func (m *Migrator) migrateParallel(ds *schema.Dataset, mig *Migration, files []string, jobs int) (int64, error) {
	type result struct {
		rows int64
		err  error
	}
	index := make(chan int)
	results := make(chan result, len(files))
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range index {
				rows, err := m.migrateOne(ds, mig, files[i], i+1)
				results <- result{rows: rows, err: err}
			}
		}()
	}
	go func() {
		for i := range files {
			index <- i
		}
		close(index)
		wg.Wait()
		close(results)
	}()
	var total int64
	var firstErr error
	for r := range results {
		total += r.rows
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
	}
	if firstErr != nil {
		return total, firstErr
	}
	return total, nil
}

// migrateOne 迁移单个源文件并写入 manifest 记录。
func (m *Migrator) migrateOne(ds *schema.Dataset, mig *Migration, file string, seq int) (int64, error) {
	parts := parsePathPartitions(file)
	rows, filesOut, err := m.migrateFile(ds, mig, file, parts)
	if err != nil {
		return rows, fmt.Errorf("migrate %s: %w", file, err)
	}
	record := lake.Batch{
		ID:         batchID(ds.Name, seq),
		Dataset:    ds.Name,
		Operation:  "migrate",
		Source:     "oldlake:" + filepath.ToSlash(file),
		CreatedAt:  nowUTC(),
		Rows:       rows,
		Partitions: partitionValuesSorted(ds, parts),
		Files:      filesOut,
	}
	if m.Logger != nil {
		if err := m.Logger.Append(record); err != nil {
			return rows, err
		}
	}
	m.logf("migrated %s rows=%d -> %v", filepath.Base(file), rows, parts)
	return rows, nil
}

func (m *Migrator) migrateFile(ds *schema.Dataset, mig *Migration, srcPath string, parts map[string]string) (int64, []lake.BatchFile, error) {
	// 打开源文件
	fh, err := os.Open(srcPath)
	if err != nil {
		return 0, nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return 0, nil, err
	}
	pf, err := parquet.OpenFile(fh, st.Size())
	if err != nil {
		return 0, nil, err
	}

	// 源列索引与时间戳缩放
	srcFields := pf.Schema().Fields()
	colIdx := make(map[string]int, len(srcFields))
	tsScale := make(map[string]int64, len(srcFields))
	for i, f := range srcFields {
		colIdx[f.Name()] = i
		tsScale[f.Name()] = pq.TimestampScale(f.Type().LogicalType())
	}

	// 校验映射完整
	for _, fm := range mig.Fields {
		if fm.Dst == "" {
			continue
		}
		if _, ok := colIdx[fm.Src]; !ok {
			return 0, nil, fmt.Errorf("source column %q not found", fm.Src)
		}
		if _, ok := ds.FieldIndex(fm.Dst); !ok {
			return 0, nil, fmt.Errorf("target field %q not in dataset %s", fm.Dst, ds.Name)
		}
	}

	// 目标分区目录
	rel, err := lake.PartitionPath(ds, parts)
	if err != nil {
		return 0, nil, err
	}
	dir := filepath.Join(m.Lake.Dir(ds), rel)
	pw, err := lake.NewPartWriter(ds, dir)
	if err != nil {
		return 0, nil, err
	}

	// 源文件按"每行组一只股票"分块,但块序随机;按代码排序后重排写出,
	// 使输出按 (ts_code, 时间) 有序,行组统计才能有效剪枝。
	order, err := m.blockOrder(pf, colIdx, filepath.Base(srcPath))
	if err != nil {
		_, _ = pw.Close()
		return 0, nil, err
	}

	var rows int64
	convert := make([]parquet.Value, len(srcFields))
	for _, rgIdx := range order {
		rg := pf.RowGroups()[rgIdx]
		rgReader := parquet.NewRowGroupReader(rg)
		buf := make([]parquet.Row, 2048)
		for {
			n, err := rgReader.ReadRows(buf)
			for i := 0; i < n; i++ {
				for j := range convert {
					convert[j] = parquet.Value{}
				}
				for _, v := range buf[i] {
					convert[int(v.Column())] = v
				}
				out := make([]schema.Value, len(ds.Fields))
				for j := range out {
					out[j] = schema.NullValue()
				}
				for _, fm := range mig.Fields {
					if fm.Dst == "" {
						continue
					}
					di, _ := ds.FieldIndex(fm.Dst)
					out[di] = convertSource(convert[colIdx[fm.Src]], fm, tsScale[fm.Src], ds.Fields[di].Type)
				}
				if err := pw.WriteRow(out); err != nil {
					_, _ = pw.Close()
					return rows, nil, err
				}
				rows++
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				_, _ = pw.Close()
				return rows, nil, err
			}
		}
	}
	filesOut, err := pw.Close()
	if err != nil {
		return rows, filesOut, err
	}
	// 文件名需要带分区相对路径,便于后续定位
	for i := range filesOut {
		filesOut[i].RelPath = filepath.ToSlash(filepath.Join(rel, filesOut[i].RelPath))
	}
	return rows, filesOut, nil
}

// blockOrder 计算行组的写出顺序:
// 当每个行组只含一只股票时按代码排序;否则保持原顺序。
func (m *Migrator) blockOrder(pf *parquet.File, srcCols map[string]int, label string) ([]int, error) {
	srcFields := pf.Schema().Fields()
	codeCol := -1
	if i, ok := srcCols["code"]; ok {
		codeCol = i
	} else if i, ok := srcCols["ts_code"]; ok {
		codeCol = i
	}
	n := len(pf.RowGroups())
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if codeCol < 0 {
		return order, nil
	}
	codeName := srcFields[codeCol].Name()
	proj := parquet.NewSchema("codes", parquet.Group{codeName: srcFields[codeCol]})
	type block struct {
		code string
		idx  int
	}
	blocks := make([]block, 0, n)
	buf := make([]parquet.Row, 512)
	for i := 0; i < n; i++ {
		rg := pf.RowGroups()[i]
		reader := parquet.NewRowGroupReader(rg, proj)
		first := ""
		multi := false
		for {
			count, err := reader.ReadRows(buf)
			for j := 0; j < count; j++ {
				row := buf[j]
				if len(row) == 0 {
					continue
				}
				code := string(row[0].ByteArray())
				if first == "" {
					first = code
					continue
				}
				if code != first {
					multi = true
				}
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
		}
		if multi {
			m.logf("%s: row group %d spans multiple codes (first=%s); keeping original order", label, i, first)
			return order, nil
		}
		blocks = append(blocks, block{code: first, idx: i})
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].code != blocks[j].code {
			return blocks[i].code < blocks[j].code
		}
		return blocks[i].idx < blocks[j].idx
	})
	for i, b := range blocks {
		order[i] = b.idx
	}
	return order, nil
}

// convertSource 把源值转换为目标逻辑值。
//
// 源文件同一字段在不同文件里可能是 DATE(INT32 天)或 TIMESTAMP(INT64 时间单位),
// 因此以 parquet 值的物理类型为准;映射声明只提供语义归类。
func convertSource(pv parquet.Value, fm FieldMap, tsScale int64, dstType schema.FieldType) schema.Value {
	if pv.IsNull() {
		return schema.NullValue()
	}
	switch fm.Kind {
	case SrcString:
		return schema.Str(string(pv.ByteArray()))
	case SrcDate, SrcTimestamp:
		var days, micros int64
		if pv.Kind() == parquet.Int32 {
			days = int64(pv.Int32())
			micros = days * 24 * 3600 * 1_000_000
		} else {
			micros = pv.Int64()
			switch {
			case tsScale == 1:
			case tsScale == -1000:
				micros /= 1000
			default:
				micros *= tsScale
			}
			days = micros / (24 * 3600 * 1_000_000)
		}
		if dstType == schema.TypeDate {
			return schema.Date(days)
		}
		return schema.Timestamp(micros)
	case SrcFloat:
		v := pv.Double()
		if math.IsNaN(v) {
			return schema.NullValue()
		}
		if fm.Scale != 0 && fm.Scale != 1 {
			v *= fm.Scale
		}
		return schema.Float(v)
	default:
		return schema.NullValue()
	}
}

// listParquet 递归枚举目录下所有 .parquet 文件。
func listParquet(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".parquet") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return files, nil
}

// parsePathPartitions 从路径中提取全部 key=value 分区段。
func parsePathPartitions(path string) map[string]string {
	out := map[string]string{}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if i := strings.IndexByte(seg, '='); i > 0 {
			out[seg[:i]] = seg[i+1:]
		}
	}
	return out
}

func partitionValuesSorted(ds *schema.Dataset, parts map[string]string) []string {
	out := make([]string, 0, len(ds.Partitions))
	for _, key := range ds.Partitions {
		if v, ok := parts[key]; ok && v != "" {
			out = append(out, key+"="+v)
		}
	}
	return out
}
