package factor

import (
	"math"
	"sort"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

// TrackingResult 是因子跟踪结果(逐日 IC + 分层收益 + 覆盖率)。
type TrackingResult struct {
	Factor      string         `json:"factor"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Direction   string         `json:"direction,omitempty"`
	Dataset     string         `json:"dataset"`
	StartDate   string         `json:"start_date"`
	EndDate     string         `json:"end_date"`
	Horizon     int            `json:"horizon"`   // 前向收益天数
	Layers      int            `json:"layers"`    // 分层数
	Dates       []string       `json:"dates"`     // 逐日
	IC          []*float64     `json:"ic"`        // 皮尔逊 IC
	RankIC      []*float64     `json:"rank_ic"`   // 斯皮尔曼 RankIC
	Coverage    []float64      `json:"coverage"`  // 当日有效因子值 / 全市场行数
	LayerRet    [][]*float64   `json:"layer_ret"` // 每层逐日平均前向收益(索引 0 = 因子值最小层)
	LayerCum    [][]float64    `json:"layer_cum"` // 每层累计收益(按日复合)
	Stats       map[string]any `json:"stats"`     // 汇总:ic_mean/ic_std/ir/rank_ic_mean/多空等
	UniverseAvg float64        `json:"universe_avg"`
	Error       string         `json:"error,omitempty"`
}

// TrackOptions 控制跟踪计算。
type TrackOptions struct {
	StartDate string // YYYYMMDD 或 YYYY-MM-DD
	EndDate   string
	Horizon   int // 前向收益天数(默认 5)
	Layers    int // 分层数(默认 5)
}

// Track 计算因子在区间内的 IC 与分层收益。
//
// 数据需求:因子窗口 + 前向收益天数 → 实际加载 [start-window, end+horizon*2]。
func Track(l *lake.Lake, def *Definition, opts TrackOptions) (*TrackingResult, error) {
	start, err := schema.ParseDate(opts.StartDate)
	if err != nil {
		return nil, err
	}
	end, err := schema.ParseDate(opts.EndDate)
	if err != nil {
		return nil, err
	}
	horizon := opts.Horizon
	if horizon <= 0 {
		horizon = 5
	}
	layers := opts.Layers
	if layers <= 0 {
		layers = 5
	}
	window := int64(def.Window())
	// 多加载一段:因子窗口回看 + 前向收益(按日历日粗放取,交易日数足够)
	lo := start - window*2 - 5
	hi := end + int64(horizon*2) + 5

	panel, err := LoadPanel(l, lo, hi)
	if err != nil {
		return nil, err
	}
	result := &TrackingResult{
		Factor:    def.ID,
		Name:      def.Name,
		Kind:      def.Kind,
		Direction: def.Direction,
		Dataset:   panel.Dataset,
		StartDate: schema.FormatDateISO(start),
		EndDate:   schema.FormatDateISO(end),
		Horizon:   horizon,
		Layers:    layers,
		Stats:     map[string]any{},
	}
	// 分层收益按层预分配(后续按日 append,不能留 nil)
	result.LayerRet = make([][]*float64, layers)
	if len(panel.Days) == 0 {
		result.Error = "区间内无截面数据"
		return result, nil
	}

	var icAll, rankICAll []float64
	layerSums := make([]float64, layers)
	layerCounts := make([]int, layers)
	universeSum, universeCount := 0.0, 0
	for _, day := range panel.Days {
		if day < start || day > end {
			continue
		}
		values := panel.Values(def, day)
		total := len(panel.ByDay[day])
		if total == 0 {
			continue
		}
		result.Coverage = append(result.Coverage, float64(len(values))/float64(total))
		result.Dates = append(result.Dates, schema.FormatDateISO(day))

		// 与同日的前向收益配对
		codes := make([]string, 0, len(values))
		fvals := make([]float64, 0, len(values))
		rets := make([]float64, 0, len(values))
		for code, value := range values {
			ret, ok := panel.ForwardReturn(code, day, horizon)
			if !ok || math.IsNaN(ret) {
				continue
			}
			codes = append(codes, code)
			fvals = append(fvals, value)
			rets = append(rets, ret)
		}
		if len(codes) < layers*2 {
			result.IC = append(result.IC, nil)
			result.RankIC = append(result.RankIC, nil)
			for i := 0; i < layers; i++ {
				result.LayerRet[i] = append(result.LayerRet[i], nil)
			}
			continue
		}
		ic := pearson(fvals, rets)
		rankIC := spearman(fvals, rets)
		result.IC = append(result.IC, floatPtr(ic))
		result.RankIC = append(result.RankIC, floatPtr(rankIC))
		if !math.IsNaN(ic) {
			icAll = append(icAll, ic)
		}
		if !math.IsNaN(rankIC) {
			rankICAll = append(rankICAll, rankIC)
		}

		// 分层:按因子值排序后等分
		order := make([]int, len(codes))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(a, b int) bool { return fvals[order[a]] < fvals[order[b]] })
		perLayer := len(order) / layers
		if perLayer == 0 {
			perLayer = 1
		}
		for layer := 0; layer < layers; layer++ {
			from := layer * perLayer
			to := from + perLayer
			if layer == layers-1 {
				to = len(order)
			}
			if from >= to {
				result.LayerRet[layer] = append(result.LayerRet[layer], nil)
				continue
			}
			sum := 0.0
			for _, idx := range order[from:to] {
				sum += rets[idx]
			}
			avg := sum / float64(to-from)
			result.LayerRet[layer] = append(result.LayerRet[layer], floatPtr(avg))
			layerSums[layer] += avg
			layerCounts[layer]++
		}
		for _, ret := range rets {
			universeSum += ret
			universeCount++
		}
	}

	// 每层累计(按日复合)
	result.LayerCum = make([][]float64, layers)
	for layer := 0; layer < layers; layer++ {
		cum := 1.0
		series := make([]float64, 0, len(result.LayerRet[layer]))
		for _, v := range result.LayerRet[layer] {
			if v != nil {
				cum *= 1 + *v/float64(horizon) // 摊平到单日,便于累计展示
			}
			series = append(series, cum-1)
		}
		result.LayerCum[layer] = series
	}

	result.Stats["ic_mean"] = meanOrNaN(icAll)
	result.Stats["ic_std"] = stddevOrNaN(icAll)
	result.Stats["ir"] = safeDiv(meanOrNaN(icAll), stddevOrNaN(icAll))
	result.Stats["ic_positive_ratio"] = positiveRatio(icAll)
	result.Stats["rank_ic_mean"] = meanOrNaN(rankICAll)
	result.Stats["rank_ic_std"] = stddevOrNaN(rankICAll)
	result.Stats["rank_ir"] = safeDiv(meanOrNaN(rankICAll), stddevOrNaN(rankICAll))
	result.Stats["samples"] = len(result.Dates)
	if universeCount > 0 {
		result.UniverseAvg = universeSum / float64(universeCount)
	}
	// 多空(最高层 - 最低层,累计口径)
	if layers >= 2 {
		top := result.LayerCum[layers-1]
		bottom := result.LayerCum[0]
		if len(top) > 0 && len(bottom) > 0 {
			result.Stats["long_short_cum"] = top[len(top)-1] - bottom[len(bottom)-1]
		}
		avgTop, avgBottom := meanOrNaN(layerSums[len(layerSums)-1:]), meanOrNaN(layerSums[:1])
		if layerCounts[layers-1] > 0 && layerCounts[0] > 0 {
			result.Stats["top_layer_avg"] = layerSums[layers-1] / float64(layerCounts[layers-1])
			result.Stats["bottom_layer_avg"] = layerSums[0] / float64(layerCounts[0])
		}
		_ = avgTop
		_ = avgBottom
	}
	return result, nil
}

// Coverage 计算因子在区间内的覆盖率(有效值/全市场)。
func Coverage(l *lake.Lake, def *Definition, start, end int64) ([]map[string]any, error) {
	window := int64(def.Window())
	panel, err := LoadPanel(l, start-window*2-5, end)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, day := range panel.Days {
		if day < start || day > end {
			continue
		}
		total := len(panel.ByDay[day])
		if total == 0 {
			continue
		}
		values := panel.Values(def, day)
		out = append(out, map[string]any{
			"date":     schema.FormatDateISO(day),
			"universe": total,
			"valid":    len(values),
			"coverage": float64(len(values)) / float64(total),
		})
	}
	return out, nil
}

// ── 统计工具 ────────────────────────────────────────────────────────────

func floatPtr(v float64) *float64 { return &v }

func meanOrNaN(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func stddevOrNaN(xs []float64) float64 {
	if len(xs) < 2 {
		return math.NaN()
	}
	m := meanOrNaN(xs)
	sum := 0.0
	for _, x := range xs {
		sum += (x - m) * (x - m)
	}
	return math.Sqrt(sum / float64(len(xs)-1))
}

func safeDiv(a, b float64) float64 {
	if b == 0 || math.IsNaN(a) || math.IsNaN(b) {
		return math.NaN()
	}
	return a / b
}

func positiveRatio(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	n := 0
	for _, x := range xs {
		if x > 0 {
			n++
		}
	}
	return float64(n) / float64(len(xs))
}

// pearson 皮尔逊相关系数。
func pearson(xs, ys []float64) float64 {
	if len(xs) != len(ys) || len(xs) < 3 {
		return math.NaN()
	}
	mx, my := meanOrNaN(xs), meanOrNaN(ys)
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return math.NaN()
	}
	return sxy / math.Sqrt(sxx*syy)
}

// spearman 秩相关(对平均秩处理并列)。
func spearman(xs, ys []float64) float64 {
	return pearson(ranks(xs), ranks(ys))
}

func ranks(xs []float64) []float64 {
	n := len(xs)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	out := make([]float64, n)
	i := 0
	for i < n {
		j := i
		for j+1 < n && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		avgRank := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = avgRank
		}
		i = j + 1
	}
	return out
}
