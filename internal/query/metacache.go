package query

import (
	"sync"

	"github.com/parquet-go/parquet-go"

	"quant-core/internal/pq"
	"quant-core/internal/schema"
)

// MetaCache 缓存 parquet 文件的行组统计,避免重复解析 footer 与页索引。
//
// 数据湖写入后文件不可变,因此缓存无需失效(仍带文件大小与修改时间校验)。
// 缓存只保存统计信息(每个行组的每列 min/max),不保存数据页。
type MetaCache struct {
	mu      sync.RWMutex
	entries map[string]*fileMeta
	order   []string
	max     int
}

type fileMeta struct {
	size    int64
	modUnix int64
	groups  []rowGroupMeta
}

type rowGroupMeta struct {
	numRows int64
	cols    map[string]colStats
}

type colStats struct {
	hasIndex bool
	min      parquet.Value
	max      parquet.Value
}

// NewMetaCache 创建缓存,max 为最多缓存的文件数(<=0 时默认 2048)。
func NewMetaCache(max int) *MetaCache {
	if max <= 0 {
		max = 2048
	}
	return &MetaCache{entries: map[string]*fileMeta{}, max: max}
}

func (c *MetaCache) get(path string, size, modUnix int64) (*fileMeta, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.RLock()
	fm, ok := c.entries[path]
	c.mu.RUnlock()
	if !ok || fm.size != size || fm.modUnix != modUnix {
		return nil, false
	}
	return fm, true
}

func (c *MetaCache) put(path string, fm *fileMeta) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[path]; !exists {
		c.order = append(c.order, path)
		if len(c.order) > c.max {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
	}
	c.entries[path] = fm
}

// Len 返回当前缓存条目数(观测用)。
func (c *MetaCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// buildFileMeta 解析文件的全部行组统计。
func buildFileMeta(pf *parquet.File, size, modUnix int64) *fileMeta {
	groups := make([]rowGroupMeta, 0, len(pf.RowGroups()))
	for _, rg := range pf.RowGroups() {
		fields := rg.Schema().Fields()
		chunks := rg.ColumnChunks()
		meta := rowGroupMeta{numRows: rg.NumRows(), cols: make(map[string]colStats, len(fields))}
		for i, field := range fields {
			if i >= len(chunks) {
				break
			}
			stats := colStats{}
			if idx, err := chunks[i].ColumnIndex(); err == nil && idx != nil && idx.NumPages() > 0 {
				first := true
				for p := 0; p < idx.NumPages(); p++ {
					if idx.NullPage(p) {
						continue
					}
					mn, mx := idx.MinValue(p).Clone(), idx.MaxValue(p).Clone()
					if first {
						stats.min, stats.max, first = mn, mx, false
						continue
					}
					if pq.CompareValues(mn, stats.min) < 0 {
						stats.min = mn
					}
					if pq.CompareValues(mx, stats.max) > 0 {
						stats.max = mx
					}
				}
				stats.hasIndex = !first
			}
			meta.cols[field.Name()] = stats
		}
		groups = append(groups, meta)
	}
	return &fileMeta{size: size, modUnix: modUnix, groups: groups}
}

// fileMetaMayMatch 判断文件是否可能包含匹配行;返回 (可能匹配, 可剪枝行组数)。
func fileMetaMayMatch(fm *fileMeta, ds *schema.Dataset, filter *Filter) (bool, int) {
	if filter == nil || len(filter.Preds) == 0 {
		return true, 0
	}
	mayMatch := false
	skippable := 0
	for i := range fm.groups {
		rg := &fm.groups[i]
		skip := false
		for _, pred := range filter.Preds {
			name := ds.Fields[pred.Field].Name
			stats, ok := rg.cols[name]
			if !ok || !stats.hasIndex {
				continue
			}
			if !statsMayMatch(stats, pred, ds.Fields[pred.Field].Type) {
				skip = true
				break
			}
		}
		if skip {
			skippable++
		} else {
			mayMatch = true
		}
	}
	return mayMatch, skippable
}

// statsMayMatch 判断单个行组的列统计是否可能与谓词匹配。
func statsMayMatch(stats colStats, p Predicate, ft schema.FieldType) bool {
	lo, hi := stats.min, stats.max
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
		values := p.inStatsValues(ft)
		i := searchParquetValues(values, hi)
		return i > 0 && pq.CompareValues(values[i-1], lo) >= 0
	default:
		return true
	}
}

// inStatsValues 返回 IN 取值的物理值(按物理类型转换并排序,首次构建后缓存)。
func (p *Predicate) inStatsValues(ft schema.FieldType) []parquet.Value {
	if p.inStatsOK && p.inStatsType == ft {
		return p.inStats
	}
	values := make([]parquet.Value, 0, len(p.Values))
	for _, v := range p.Values {
		values = append(values, pq.ToParquet(v, ft))
	}
	sortParquetValues(values)
	p.inStats, p.inStatsType, p.inStatsOK = values, ft, true
	return values
}

func sortParquetValues(values []parquet.Value) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && pq.CompareValues(values[j], values[j-1]) < 0; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// searchParquetValues 返回第一个大于 v 的下标。
func searchParquetValues(values []parquet.Value, v parquet.Value) int {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := (lo + hi) / 2
		if pq.CompareValues(values[mid], v) > 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}
