package factor

import (
	"fmt"
	"math"
	"sort"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// CrossRow 是截面表的一行(计算因子所需字段)。
type CrossRow struct {
	Code         string
	Day          int64
	Close        float64
	PctChg       float64
	TurnoverRate float64
	Pe           float64
	PeTTM        float64
	Pb           float64
	PsTTM        float64
	DvTTM        float64
	TotalShare   float64
	FloatShare   float64
	TotalMV      float64
	CircMV       float64
}

// Series 是单只证券的日序列(按日期升序)。
type Series struct {
	Days     []int64
	Close    []float64
	PctChg   []float64
	Turnover []float64
	TotalMV  []float64
	PeTTM    []float64
	Pb       []float64
	PsTTM    []float64
	DvTTM    []float64
	CircMV   []float64
}

// Panel 是一次加载的截面数据:按日索引 + 按证券索引。
type Panel struct {
	Days    []int64
	ByDay   map[int64][]CrossRow
	ByCode  map[string]*Series
	MinDay  int64
	MaxDay  int64
	Rows    int
	Dataset string
}

// LoadPanel 加载 [lo, hi] 区间内的截面数据(优先 daily_cross,缺失年份自动回退 bars_daily)。
func LoadPanel(l *lake.Lake, lo, hi int64) (*Panel, error) {
	ds, err := pickDataset(l, lo, hi)
	if err != nil {
		return nil, err
	}
	scanner := query.NewCachedScanner(l, 64)
	codeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	cur, err := scanner.Open(query.Request{
		Dataset: ds.Name,
		Columns: []string{"ts_code", "trade_date", "close", "pct_chg", "turnover_rate",
			"pe", "pe_ttm", "pb", "ps_ttm", "dv_ttm", "total_share", "float_share", "total_mv", "circ_mv"},
		Filter: &query.Filter{Preds: []query.Predicate{
			query.Gte(dateIdx, schema.Date(lo)),
			query.Lte(dateIdx, schema.Date(hi)),
		}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close()
	panel := &Panel{
		ByDay:   map[int64][]CrossRow{},
		ByCode:  map[string]*Series{},
		MinDay:  lo,
		MaxDay:  hi,
		Dataset: ds.Name,
	}
	daySeen := map[int64]bool{}
	for cur.Next() {
		row := cur.Row()
		rec := CrossRow{
			Code:         row[0].S,
			Day:          row[1].I,
			Close:        num(row[2]),
			PctChg:       num(row[3]),
			TurnoverRate: num(row[4]),
			Pe:           num(row[5]),
			PeTTM:        num(row[6]),
			Pb:           num(row[7]),
			PsTTM:        num(row[8]),
			DvTTM:        num(row[9]),
			TotalShare:   num(row[10]),
			FloatShare:   num(row[11]),
			TotalMV:      num(row[12]),
			CircMV:       num(row[13]),
		}
		panel.ByDay[rec.Day] = append(panel.ByDay[rec.Day], rec)
		panel.Rows++
		if !daySeen[rec.Day] {
			daySeen[rec.Day] = true
			panel.Days = append(panel.Days, rec.Day)
		}
		series := panel.ByCode[rec.Code]
		if series == nil {
			series = &Series{}
			panel.ByCode[rec.Code] = series
		}
		series.Days = append(series.Days, rec.Day)
		series.Close = append(series.Close, rec.Close)
		series.PctChg = append(series.PctChg, rec.PctChg)
		series.Turnover = append(series.Turnover, rec.TurnoverRate)
		series.TotalMV = append(series.TotalMV, rec.TotalMV)
		series.PeTTM = append(series.PeTTM, rec.PeTTM)
		series.Pb = append(series.Pb, rec.Pb)
		series.PsTTM = append(series.PsTTM, rec.PsTTM)
		series.DvTTM = append(series.DvTTM, rec.DvTTM)
		series.CircMV = append(series.CircMV, rec.CircMV)
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	sort.Slice(panel.Days, func(i, j int) bool { return panel.Days[i] < panel.Days[j] })
	for _, series := range panel.ByCode {
		sortSeries(series)
	}
	_ = codeIdx
	return panel, nil
}

// pickDataset 优先用 daily_cross(按日排序),该区间无数据时回退 bars_daily。
func pickDataset(l *lake.Lake, lo, hi int64) (*schema.Dataset, error) {
	if ds, err := l.Registry.Get("daily_cross"); err == nil {
		files, ferr := l.WalkFiles(ds, func(values map[string]string, depth int) bool {
			year := values["year"]
			if year == "" {
				return true
			}
			for d := lo; d <= hi; d += 1 {
				break
			}
			return true
		})
		if ferr == nil && len(files) > 0 {
			return ds, nil
		}
	}
	return l.Registry.Get("bars_daily")
}

func sortSeries(s *Series) {
	idx := make([]int, len(s.Days))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return s.Days[idx[a]] < s.Days[idx[b]] })
	perm := func(in []float64) []float64 {
		out := make([]float64, len(in))
		for i, j := range idx {
			out[i] = in[j]
		}
		return out
	}
	days := make([]int64, len(s.Days))
	for i, j := range idx {
		days[i] = s.Days[j]
	}
	s.Days = days
	s.Close, s.PctChg, s.Turnover, s.TotalMV = perm(s.Close), perm(s.PctChg), perm(s.Turnover), perm(s.TotalMV)
	s.PeTTM, s.Pb, s.PsTTM, s.DvTTM, s.CircMV = perm(s.PeTTM), perm(s.Pb), perm(s.PsTTM), perm(s.DvTTM), perm(s.CircMV)
}

// indexOf 返回天数在序列中的下标(不存在返回 -1)。
func indexOf(days []int64, day int64) int {
	i := sort.Search(len(days), func(i int) bool { return days[i] >= day })
	if i < len(days) && days[i] == day {
		return i
	}
	return -1
}

// Values 计算某因子的当日截面值:code → 值(NaN 表示缺失)。
func (p *Panel) Values(def *Definition, day int64) map[string]float64 {
	rows := p.ByDay[day]
	if len(rows) == 0 {
		return nil
	}
	out := make(map[string]float64, len(rows))
	window := def.Window()
	for _, row := range rows {
		series := p.ByCode[row.Code]
		if series == nil {
			continue
		}
		idx := indexOf(series.Days, day)
		if idx < 0 {
			continue
		}
		var value float64
		ok := true
		switch def.Kind {
		case "momentum", "reversal":
			if idx-window < 0 {
				ok = false
				break
			}
			base := series.Close[idx-window]
			if base <= 0 {
				ok = false
				break
			}
			value = series.Close[idx]/base - 1
			if def.Kind == "reversal" {
				value = -value
			}
		case "volatility":
			if idx-window < 0 {
				ok = false
				break
			}
			value = stddev(series.PctChg[idx-window+1 : idx+1])
		case "turnover":
			if idx-window < 0 {
				ok = false
				break
			}
			value = mean(series.Turnover[idx-window+1 : idx+1])
		case "size":
			value = row.TotalMV
			if value <= 0 {
				ok = false
				break
			}
			if def.LogSize() {
				value = math.Log(value)
			}
		case "field":
			switch def.Field() {
			case "pb":
				value = row.Pb
			case "pe_ttm":
				value = row.PeTTM
			case "ps_ttm":
				value = row.PsTTM
			case "dv_ttm":
				value = row.DvTTM
			case "total_mv":
				value = row.TotalMV
			case "circ_mv":
				value = row.CircMV
			case "turnover_rate":
				value = row.TurnoverRate
			case "close":
				value = row.Close
			default:
				ok = false
			}
		default:
			ok = false
		}
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		out[row.Code] = value
	}
	return out
}

// ForwardReturn 计算前向收益:close[day+h]/close[day]-1(NaN 表示缺失)。
func (p *Panel) ForwardReturn(code string, day int64, horizon int) (float64, bool) {
	series := p.ByCode[code]
	if series == nil {
		return 0, false
	}
	idx := indexOf(series.Days, day)
	if idx < 0 || idx+horizon >= len(series.Days) {
		return 0, false
	}
	base := series.Close[idx]
	if base <= 0 {
		return 0, false
	}
	return series.Close[idx+horizon]/base - 1, true
}

func num(v schema.Value) float64 {
	if v.Kind == schema.KindFloat {
		return v.F
	}
	return 0
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return math.NaN()
	}
	m := mean(xs)
	sum := 0.0
	for _, x := range xs {
		sum += (x - m) * (x - m)
	}
	return math.Sqrt(sum / float64(len(xs)-1))
}

// String 便于错误信息。
func (p *Panel) String() string {
	return fmt.Sprintf("panel(dataset=%s days=%d codes=%d rows=%d)", p.Dataset, len(p.Days), len(p.ByCode), p.Rows)
}
