package query

import "quant-core/internal/schema"

// Op 是谓词操作符。
type Op uint8

const (
	OpEq Op = iota // 等于
	OpIn           // 属于集合
	OpGte          // 大于等于
	OpLte          // 小于等于
	OpGt           // 大于
	OpLt           // 小于
)

// Predicate 是单字段上的过滤条件。Field 是数据集字段索引。
type Predicate struct {
	Field  int
	Op     Op
	Value  schema.Value   // OpEq/OpGte/OpLte/OpGt/OpLt
	Values []schema.Value // OpIn
}

// Filter 是谓词的合取(AND)。
type Filter struct {
	Preds []Predicate
}

// Eq 构建等于谓词。
func Eq(field int, v schema.Value) Predicate { return Predicate{Field: field, Op: OpEq, Value: v} }

// In 构建集合谓词。
func In(field int, vs ...schema.Value) Predicate {
	return Predicate{Field: field, Op: OpIn, Values: vs}
}

// Gte 构建 >= 谓词。
func Gte(field int, v schema.Value) Predicate { return Predicate{Field: field, Op: OpGte, Value: v} }

// Lte 构建 <= 谓词。
func Lte(field int, v schema.Value) Predicate { return Predicate{Field: field, Op: OpLte, Value: v} }

// Gt 构建 > 谓词。
func Gt(field int, v schema.Value) Predicate { return Predicate{Field: field, Op: OpGt, Value: v} }

// Lt 构建 < 谓词。
func Lt(field int, v schema.Value) Predicate { return Predicate{Field: field, Op: OpLt, Value: v} }

// Match 判断行是否满足全部谓词。row 使用与谓词一致的字段索引。
func (f *Filter) Match(row []schema.Value) bool {
	return f.matchWith(row, func(i int) int { return i })
}

// MatchPos 判断行是否满足全部谓词,pos 把谓词的字段索引映射到行位置。
func (f *Filter) MatchPos(row []schema.Value, pos func(int) int) bool {
	return f.matchWith(row, pos)
}

func (f *Filter) matchWith(row []schema.Value, pos func(int) int) bool {
	if f == nil {
		return true
	}
	for i := range f.Preds {
		if !f.Preds[i].matchAt(row, pos(f.Preds[i].Field)) {
			return false
		}
	}
	return true
}

func (p *Predicate) matchAt(row []schema.Value, pos int) bool {
	if pos < 0 || pos >= len(row) {
		return false
	}
	v := row[pos]
	if v.IsNull() {
		return false
	}
	switch p.Op {
	case OpEq:
		c, err := schema.Compare(v, p.Value)
		return err == nil && c == 0
	case OpIn:
		for _, x := range p.Values {
			if c, err := schema.Compare(v, x); err == nil && c == 0 {
				return true
			}
		}
		return false
	case OpGte:
		c, err := schema.Compare(v, p.Value)
		return err == nil && c >= 0
	case OpLte:
		c, err := schema.Compare(v, p.Value)
		return err == nil && c <= 0
	case OpGt:
		c, err := schema.Compare(v, p.Value)
		return err == nil && c > 0
	case OpLt:
		c, err := schema.Compare(v, p.Value)
		return err == nil && c < 0
	default:
		return false
	}
}

