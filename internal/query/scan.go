package query

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/lake"
	"quant-core/internal/pq"
	"quant-core/internal/schema"
)

// Scanner 在数据湖上执行流式扫描,是读取湖数据的唯一实现。
type Scanner struct {
	Lake *lake.Lake
}

func NewScanner(l *lake.Lake) *Scanner { return &Scanner{Lake: l} }

// Request 描述一次扫描。Filter 为 nil 时表示全量;Columns 为 nil 时返回全部字段。
type Request struct {
	Dataset string
	Filter  *Filter
	Columns []string
	Limit   int
	Offset  int
}

// batchRows 是每次从 parquet 读取的行数。
const batchRows = 512

// microsPerDay 是一天的微秒数。
const microsPerDay = int64(86400) * 1_000_000

// Cursor 是流式扫描游标。典型用法:
//
//	cur, err := scanner.Open(req)
//	defer cur.Close()
//	for cur.Next() { row := cur.Row() }
//	if err := cur.Err(); err != nil { ... }
type Cursor struct {
	ds       *schema.Dataset
	filter   *Filter
	columns  []int // 输出列(数据集字段索引),按请求顺序
	readCols []int // 实际读取列(输出列 ∪ 过滤列),按字段名字典序
	readPos  map[int]int
	tsScale  []int64

	proj    *parquet.Schema
	files   []lake.FileRef
	fileIdx int

	file   *os.File
	pf     *parquet.File
	colIdx map[string]int
	rgIdx  int
	reader *parquet.Reader

	buf    []parquet.Row
	bufN   int
	bufPos int

	row     []schema.Value
	limit   int
	offset  int
	emitted int
	seen    int
	err     error
	closed  bool

	stats Stats
}

// Stats 是扫描过程的观测统计。
type Stats struct {
	FilesOpened      int   `json:"files_opened"`
	RowGroupsRead    int   `json:"row_groups_read"`
	RowGroupsSkipped int   `json:"row_groups_skipped"`
	RowsSeen         int64 `json:"rows_seen"`
}

// Open 打开一个扫描游标。
func (s *Scanner) Open(req Request) (*Cursor, error) {
	ds, err := s.Lake.Registry.Get(req.Dataset)
	if err != nil {
		return nil, err
	}
	c := &Cursor{ds: ds, filter: req.Filter, limit: req.Limit, offset: req.Offset}

	// 输出列
	if len(req.Columns) == 0 {
		c.columns = make([]int, len(ds.Fields))
		for i := range ds.Fields {
			c.columns[i] = i
		}
	} else {
		for _, name := range req.Columns {
			i, ok := ds.FieldIndex(name)
			if !ok {
				return nil, fmt.Errorf("dataset %s: unknown column %q", ds.Name, name)
			}
			c.columns = append(c.columns, i)
		}
	}

	// 读取列 = 输出列 ∪ 过滤列
	readSet := map[int]bool{}
	for _, i := range c.columns {
		readSet[i] = true
	}
	if c.filter != nil {
		for _, p := range c.filter.Preds {
			if p.Field < 0 || p.Field >= len(ds.Fields) {
				return nil, fmt.Errorf("filter field index %d out of range", p.Field)
			}
			readSet[p.Field] = true
		}
	}
	for i := range readSet {
		c.readCols = append(c.readCols, i)
	}

	// 分区过滤 + 文件枚举
	accept, err := partitionAccept(ds, c.filter)
	if err != nil {
		return nil, err
	}
	c.files, err = s.Lake.WalkFiles(ds, accept)
	if err != nil {
		return nil, err
	}
	c.buf = make([]parquet.Row, batchRows)
	return c, nil
}

// Next 前进到下一行,无更多数据时返回 false。
func (c *Cursor) Next() bool {
	for {
		if c.err != nil || c.closed {
			return false
		}
		if c.limit > 0 && c.emitted >= c.limit {
			return false
		}
		// 1) 从当前缓冲取行
		if c.bufPos < c.bufN {
			prow := c.buf[c.bufPos]
			c.bufPos++
			row, err := c.convertRow(prow)
			if err != nil {
				c.err = err
				return false
			}
			c.stats.RowsSeen++
			if c.filter != nil && !c.filter.MatchPos(row, func(field int) int { return c.readPos[field] }) {
				continue
			}
			if c.seen < c.offset {
				c.seen++
				continue
			}
			c.seen++
			c.row = row
			c.emitted++
			return true
		}
		// 2) 继续读当前行组
		if c.reader != nil {
			n, err := c.reader.ReadRows(c.buf)
			if n > 0 {
				c.bufN, c.bufPos = n, 0
				continue
			}
			if err != nil && err != io.EOF {
				c.err = err
				return false
			}
			c.reader = nil
			continue
		}
		// 3) 推进到下一个行组/文件
		if !c.advance() {
			return false
		}
	}
}

// Row 返回当前行,长度为请求的输出列数。
func (c *Cursor) Row() []schema.Value {
	if c.row == nil {
		return nil
	}
	out := make([]schema.Value, len(c.columns))
	for i, ci := range c.columns {
		out[i] = c.row[c.readPos[ci]]
	}
	return out
}

// Err 返回游标错误。
func (c *Cursor) Err() error { return c.err }

// Stats 返回扫描统计。
func (c *Cursor) Stats() Stats { return c.stats }

// Close 释放全部资源。
func (c *Cursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.reader = nil
	if c.file != nil {
		err := c.file.Close()
		c.file = nil
		return err
	}
	return nil
}

// advance 打开下一个可读行组,必要时切换到下一个文件。返回 false 表示扫描结束。
func (c *Cursor) advance() bool {
	for {
		if c.pf != nil && c.rgIdx < len(c.pf.RowGroups()) {
			rg := c.pf.RowGroups()[c.rgIdx]
			c.rgIdx++
			if c.rowGroupSkippable(rg) {
				c.stats.RowGroupsSkipped++
				continue
			}
			c.stats.RowGroupsRead++
			if c.proj == nil {
				if err := c.buildProjection(rg.Schema()); err != nil {
					c.err = err
					return false
				}
			}
			c.reader = parquet.NewRowGroupReader(rg, c.proj)
			return true
		}
		if c.file != nil {
			_ = c.file.Close()
			c.file, c.pf = nil, nil
		}
		if c.fileIdx >= len(c.files) {
			return false
		}
		ref := c.files[c.fileIdx]
		c.fileIdx++
		fh, err := os.Open(ref.Path)
		if err != nil {
			c.err = err
			return false
		}
		st, err := fh.Stat()
		if err != nil {
			_ = fh.Close()
			c.err = err
			return false
		}
		pf, err := parquet.OpenFile(fh, st.Size())
		if err != nil {
			_ = fh.Close()
			c.err = fmt.Errorf("open %s: %w", ref.Path, err)
			return false
		}
		c.file, c.pf, c.rgIdx = fh, pf, 0
		c.colIdx = nil
		c.proj = nil
		c.reader = nil
		c.stats.FilesOpened++
	}
}

// buildProjection 构建列投影 schema,并计算各读列的 timestamp 缩放。
//
// parquet.Group 构建出的 schema 按字段名字典序输出列,因此这里把 readCols
// 重排为同样的字典序,使 readCols 下标与投影行中的列位置一一对应。
func (c *Cursor) buildProjection(fileSchema *parquet.Schema) error {
	byName := make(map[string]parquet.Field)
	for _, f := range fileSchema.Fields() {
		byName[f.Name()] = f
	}
	readCols := make([]int, len(c.readCols))
	copy(readCols, c.readCols)
	sort.Slice(readCols, func(i, j int) bool {
		return c.ds.Fields[readCols[i]].Name < c.ds.Fields[readCols[j]].Name
	})
	group := parquet.Group{}
	tsScale := make([]int64, len(readCols))
	for pos, ci := range readCols {
		name := c.ds.Fields[ci].Name
		field, ok := byName[name]
		if !ok {
			return fmt.Errorf("dataset %s: file schema missing field %q", c.ds.Name, name)
		}
		group[name] = field
		if c.ds.Fields[ci].Type == schema.TypeTimestamp {
			tsScale[pos] = pq.TimestampScale(field.Type().LogicalType())
		}
	}
	c.proj = parquet.NewSchema("projection", group)
	c.readCols = readCols
	c.tsScale = tsScale
	c.readPos = make(map[int]int, len(readCols))
	for pos, ci := range readCols {
		c.readPos[ci] = pos
	}
	return nil
}

// convertRow 把 parquet 行转为逻辑值行。
//
// parquet.Row 对 optional 列可能省略 null 值,因此按 Value.Column() 定位,
// 未出现的列保持 null。
func (c *Cursor) convertRow(prow parquet.Row) ([]schema.Value, error) {
	row := make([]schema.Value, len(c.readCols))
	for _, pv := range prow {
		col := int(pv.Column())
		if col < 0 || col >= len(c.readCols) {
			return nil, fmt.Errorf("row column %d out of range [0,%d)", col, len(c.readCols))
		}
		ft := c.ds.Fields[c.readCols[col]].Type
		v, err := pq.FromParquet(pv, ft, c.tsScale[col])
		if err != nil {
			return nil, err
		}
		row[col] = v
	}
	return row, nil
}

// rowGroupSkippable 用列统计判断行组是否可能包含匹配行。
func (c *Cursor) rowGroupSkippable(rg parquet.RowGroup) bool {
	if c.filter == nil || len(c.filter.Preds) == 0 {
		return false
	}
	if c.colIdx == nil {
		c.colIdx = make(map[string]int)
		for i, f := range rg.Schema().Fields() {
			c.colIdx[f.Name()] = i
		}
	}
	chunks := rg.ColumnChunks()
	for _, pred := range c.filter.Preds {
		name := c.ds.Fields[pred.Field].Name
		ci, ok := c.colIdx[name]
		if !ok || ci >= len(chunks) {
			continue
		}
		idx, err := chunks[ci].ColumnIndex()
		if err != nil || idx == nil || idx.NumPages() == 0 {
			continue // 没有页索引,保守处理
		}
		if !pred.statsMayMatch(idx, c.ds.Fields[pred.Field].Type) {
			return true
		}
	}
	return false
}

// partitionAccept 根据过滤条件推导分区剪枝函数。
func partitionAccept(ds *schema.Dataset, filter *Filter) (func(map[string]string, int) bool, error) {
	if filter == nil {
		return nil, nil
	}
	// 时间范围缺省端的饱和边界(约 1860 ~ 2243 年),避免无限范围。
	const (
		daysMin = -40000
		daysMax = 100000
	)
	timeField := ds.TimeField()
	minDays, maxDays := int64(daysMin), int64(daysMax)
	hasRange := false
	static := map[string]map[string]bool{}
	for _, pred := range filter.Preds {
		fieldName := ds.Fields[pred.Field].Name
		if ds.HasPartition(fieldName) {
			if static[fieldName] == nil {
				static[fieldName] = map[string]bool{}
			}
			switch pred.Op {
			case OpEq:
				static[fieldName][pred.Value.S] = true
			case OpIn:
				for _, v := range pred.Values {
					static[fieldName][v.S] = true
				}
			}
		}
		if fieldName != timeField || timeField == "" {
			continue
		}
		ft := ds.Fields[pred.Field].Type
		toDays := func(v schema.Value) int64 {
			if ft == schema.TypeTimestamp {
				return v.I / microsPerDay
			}
			return v.I
		}
		switch pred.Op {
		case OpGte:
			if d := toDays(pred.Value); d > minDays {
				minDays = d
			}
			hasRange = true
		case OpLte:
			if d := toDays(pred.Value); d < maxDays {
				maxDays = d
			}
			hasRange = true
		case OpEq:
			minDays, maxDays = toDays(pred.Value), toDays(pred.Value)
			hasRange = true
		case OpIn:
			for i, v := range pred.Values {
				d := toDays(v)
				if i == 0 || d < minDays {
					minDays = d
				}
				if i == 0 || d > maxDays {
					maxDays = d
				}
			}
			hasRange = true
		}
	}
	if !hasRange && len(static) == 0 {
		return nil, nil
	}
	years := map[string]bool{}
	months := map[string]bool{} // 键为 "YYYY-MM"
	if hasRange {
		lo := schema.TimeFromDays(minDays)
		hi := schema.TimeFromDays(maxDays)
		if hi.Before(lo) {
			// 范围为空,全部剪掉
			return func(map[string]string, int) bool { return false }, nil
		}
		for y := lo.Year(); y <= hi.Year(); y++ {
			years[fmt.Sprintf("%04d", y)] = true
		}
		if ds.HasPartition("month") {
			for cursor := lo; !cursor.After(hi); cursor = cursor.AddDate(0, 1, 0) {
				months[fmt.Sprintf("%04d-%02d", cursor.Year(), cursor.Month())] = true
			}
		}
	}
	return func(values map[string]string, depth int) bool {
		key := ds.Partitions[depth]
		value, ok := values[key]
		if !ok {
			return true
		}
		switch key {
		case "year":
			if hasRange && !years[value] {
				return false
			}
			return true
		case "month":
			if hasRange {
				if year := values["year"]; year != "" {
					return months[year+"-"+value]
				}
			}
			return true
		default:
			if allowed, seen := static[key]; seen {
				return allowed[value]
			}
			return true
		}
	}, nil
}

// statsMayMatch 判断列统计区间是否可能与谓词匹配。
func (p Predicate) statsMayMatch(idx parquet.ColumnIndex, ft schema.FieldType) bool {
	var lo, hi parquet.Value
	first := true
	for i := 0; i < idx.NumPages(); i++ {
		if idx.NullPage(i) {
			continue
		}
		mn, mx := idx.MinValue(i), idx.MaxValue(i)
		if first {
			lo, hi, first = mn, mx, false
			continue
		}
		if pq.CompareValues(mn, lo) < 0 {
			lo = mn
		}
		if pq.CompareValues(mx, hi) > 0 {
			hi = mx
		}
	}
	if first {
		return true
	}
	switch p.Op {
	case OpGte:
		v := pq.ToParquet(p.Value, ft)
		return pq.CompareValues(hi, v) >= 0
	case OpLte:
		v := pq.ToParquet(p.Value, ft)
		return pq.CompareValues(lo, v) <= 0
	case OpGt:
		v := pq.ToParquet(p.Value, ft)
		return pq.CompareValues(hi, v) > 0
	case OpLt:
		v := pq.ToParquet(p.Value, ft)
		return pq.CompareValues(lo, v) < 0
	case OpEq:
		v := pq.ToParquet(p.Value, ft)
		return pq.CompareValues(lo, v) <= 0 && pq.CompareValues(v, hi) <= 0
	case OpIn:
		values := make([]parquet.Value, 0, len(p.Values))
		for _, v := range p.Values {
			values = append(values, pq.ToParquet(v, ft))
		}
		sort.Slice(values, func(i, j int) bool { return pq.CompareValues(values[i], values[j]) < 0 })
		i := sort.Search(len(values), func(i int) bool { return pq.CompareValues(values[i], hi) > 0 })
		return i > 0 && pq.CompareValues(values[i-1], lo) >= 0
	default:
		return true
	}
}
