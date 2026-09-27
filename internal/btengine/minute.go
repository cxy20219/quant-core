package btengine

import (
	"math"
	"sort"
	"sync"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// minuteSlots 是一个交易日的 240 根分钟 Bar(收盘时刻):
// 上午 09:31-11:30,下午 13:01-15:00;13:00 不产生 Bar(与 PTrade 一致)。
var minuteSlots = func() []int {
	slots := make([]int, 0, 240)
	for h := 9; h <= 11; h++ {
		start, end := 0, 60
		if h == 9 {
			start = 31
		}
		if h == 11 {
			end = 31 // 11:30 收盘
		}
		for m := start; m < end; m++ {
			slots = append(slots, h*3600+m*60)
		}
	}
	for h := 13; h <= 15; h++ {
		start, end := 0, 60
		if h == 13 {
			start = 1 // 13:00 不触发,首根为 13:01
		}
		if h == 15 {
			end = 1 // 15:00 收盘
		}
		for m := start; m < end; m++ {
			slots = append(slots, h*3600+m*60)
		}
	}
	return slots
}()

// MinuteBar 是一根分钟 Bar。
type MinuteBar struct {
	Micros int64
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
	Amount float64
}

// MinutePortal 提供分钟级回测的数据访问:按交易日滚动窗口加载 bars_1m。
//
// 设计:窗口 = 预热天数 + 当前日;推进到新交易日时增量加载并淘汰旧日,
// 内存占用与"预热天数 × 240 × 股票池"成正比,与回测总长度无关。
type MinutePortal struct {
	lake       *lake.Lake
	scanner    *query.Scanner
	securities []string
	bars       map[string][]MinuteBar
	loaded     []int64
	windowDays int
	// maxRows 是窗口内分钟 Bar 总数的上限(按池大小自动收缩窗口,防止大池长窗口吃爆内存)。
	maxRows int
	// clamped 记录窗口是否被上限收缩(用于日志提示)。
	clamped   bool
	prevClose func(sec string, day int64) float64
	calendar  func(lo, hi int64) []int64
	mu        sync.Mutex
}

// DefaultMinuteMaxRows 是分钟窗口 Bar 总数的默认上限(约 170MB 原始 Bar 数据)。
const DefaultMinuteMaxRows = 3_000_000

// NewMinutePortal 创建分钟数据门户。windowDays 是保留的交易日窗口(含预热);
// maxRows>0 时按池大小自动收缩窗口(每只每日 240 Bar)。
func NewMinutePortal(l *lake.Lake, securities []string, windowDays, maxRows int) *MinutePortal {
	if windowDays < 2 {
		windowDays = 2
	}
	if maxRows <= 0 {
		maxRows = DefaultMinuteMaxRows
	}
	return &MinutePortal{
		lake:       l,
		scanner:    query.NewCachedScanner(l, 256),
		securities: append([]string(nil), securities...),
		bars:       map[string][]MinuteBar{},
		windowDays: windowDays,
		maxRows:    maxRows,
	}
}

// effectiveWindowDays 返回按 maxRows 收缩后的窗口天数(至少 1 天)。
func (p *MinutePortal) effectiveWindowDays() int {
	days := p.windowDays
	if p.maxRows > 0 && len(p.securities) > 0 {
		perDay := len(p.securities) * len(minuteSlots)
		if perDay > 0 {
			byRows := p.maxRows / perDay
			if byRows < 1 {
				byRows = 1
			}
			if byRows < days {
				days = byRows
				p.clamped = true
			}
		}
	}
	if days < 1 {
		days = 1
	}
	return days
}

// Clamped 返回窗口是否因内存上限被收缩(供调用方提示)。
func (p *MinutePortal) Clamped() bool { return p.clamped }

// SetSecurities 更新股票池(策略动态切换时调用),已加载数据保留。
func (p *MinutePortal) SetSecurities(securities []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.securities = append([]string(nil), securities...)
	// 池变大时窗口可能被上限收缩:立即按新窗口裁剪已加载数据
	if len(p.loaded) > p.effectiveWindowDays() {
		cut := p.loaded[len(p.loaded)-p.effectiveWindowDays()]
		p.trimBefore(cut)
	}
}

// SetPrevCloseProvider 注入"最近有效日收盘价"查询(窗口内完全没有分钟数据的停牌证券使用)。
func (p *MinutePortal) SetPrevCloseProvider(fn func(sec string, day int64) float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prevClose = fn
}

// SetCalendar 注入交易日历查询(用于生成分钟日程,避免周末/假日产生虚假 Bar)。
func (p *MinutePortal) SetCalendar(fn func(lo, hi int64) []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calendar = fn
}

// EnsureDay 确保指定交易日及其之前 windowDays-1 天的分钟数据已加载。
func (p *MinutePortal) EnsureDay(day int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.loaded) > 0 && p.loaded[len(p.loaded)-1] >= day {
		return nil
	}
	window := int64(p.effectiveWindowDays())
	lo := day
	if len(p.loaded) == 0 {
		lo = day - window + 1
	} else {
		lo = p.loaded[len(p.loaded)-1] + 1
	}
	return p.loadRange(lo, day)
}

// loadRange 加载 [lo, hi] 的分钟数据(按月分区查询),并淘汰窗口外的旧数据。
func (p *MinutePortal) loadRange(lo, hi int64) error {
	ds, err := p.lake.Registry.Get("bars_1m")
	if err != nil {
		return err
	}
	codeIdx, _ := ds.FieldIndex("ts_code")
	timeIdx, _ := ds.FieldIndex("trade_time")
	loMicros := lo * 86400 * 1_000_000
	hiMicros := hi*86400*1_000_000 + 86399*1_000_000 + 999999
	cur, err := p.scanner.Open(query.Request{
		Dataset: "bars_1m",
		Columns: []string{"ts_code", "trade_time", "open", "high", "low", "close", "vol", "amount"},
		Filter: &query.Filter{Preds: []query.Predicate{
			query.In(codeIdx, toValues(p.securities)...),
			query.Gte(timeIdx, schema.Timestamp(loMicros)),
			query.Lte(timeIdx, schema.Timestamp(hiMicros)),
		}},
	})
	if err != nil {
		return err
	}
	defer cur.Close()
	for cur.Next() {
		row := cur.Row()
		sec := row[0].S
		// PTrade 单位:成交量 手→股(×100),成交额 千元→元(×1000,并消除浮点噪声)
		p.bars[sec] = append(p.bars[sec], MinuteBar{
			Micros: row[1].I,
			Open:   num(row[2]),
			High:   num(row[3]),
			Low:    num(row[4]),
			Close:  num(row[5]),
			Volume: num(row[6]) * 100,
			Amount: yuanFromThousand(num(row[7])),
		})
	}
	if err := cur.Err(); err != nil {
		return err
	}
	// 折叠开盘集合竞价 + 补齐停牌分钟(与 quantbt 的 _fold_opening_auction/_fill_minute_suspensions 一致)
	for sec, bars := range p.bars {
		p.bars[sec] = foldAuction(bars)
	}
	// 记录已加载交易日(仅交易日,供分钟日程与停牌填充使用)并淘汰窗口外数据
	days := p.calendarDays(lo, hi)
	p.loaded = append(p.loaded, days...)
	sort.Slice(p.loaded, func(i, j int) bool { return p.loaded[i] < p.loaded[j] })
	p.loaded = dedupeInt64(p.loaded)
	for sec, bars := range p.bars {
		p.bars[sec] = p.fillSuspensions(sec, bars)
	}
	window := p.effectiveWindowDays()
	if len(p.loaded) > window {
		cut := p.loaded[len(p.loaded)-window]
		p.trimBefore(cut)
	}
	return nil
}

// trimBefore 丢弃 cut 日之前的所有分钟 Bar,并把窗口列表裁到同一位置。
func (p *MinutePortal) trimBefore(cut int64) {
	cutMicros := cut * 86400 * 1_000_000
	for sec, bars := range p.bars {
		keep := bars[:0]
		for _, bar := range bars {
			if bar.Micros >= cutMicros {
				keep = append(keep, bar)
			}
		}
		p.bars[sec] = keep
	}
	for i, d := range p.loaded {
		if d >= cut {
			p.loaded = p.loaded[i:]
			break
		}
	}
}

// BarAt 返回指定时刻的分钟 Bar(精确匹配)。
func (p *MinutePortal) BarAt(sec string, micros int64) (MinuteBar, bool) {
	bars := p.bars[sec]
	idx := sort.Search(len(bars), func(i int) bool { return bars[i].Micros >= micros })
	if idx < len(bars) && bars[idx].Micros == micros {
		return bars[idx], true
	}
	return MinuteBar{}, false
}

// BarsUpTo 返回截止 endMicros(含)的最近 count 根分钟 Bar(按时间升序)。
func (p *MinutePortal) BarsUpTo(sec string, endMicros int64, count int) []MinuteBar {
	bars := p.bars[sec]
	idx := sort.Search(len(bars), func(i int) bool { return bars[i].Micros > endMicros })
	if idx == 0 {
		return nil
	}
	start := idx - count
	if start < 0 {
		start = 0
	}
	return bars[start:idx]
}

// BarsBetween 返回 [lo, hi] 区间内的分钟 Bar(按时间升序)。
func (p *MinutePortal) BarsBetween(sec string, lo, hi int64) []MinuteBar {
	bars := p.bars[sec]
	from := sort.Search(len(bars), func(i int) bool { return bars[i].Micros >= lo })
	to := sort.Search(len(bars), func(i int) bool { return bars[i].Micros > hi })
	if from >= to {
		return nil
	}
	return bars[from:to]
}

// DayHasData 判断该交易日在窗口内是否有任何数据。
func (p *MinutePortal) HasDay(day int64) bool {
	for _, d := range p.loaded {
		if d == day {
			return true
		}
	}
	return false
}

// TradingMinutes 返回回测区间的全部分钟时刻(micros,升序)。
// 时钟由日线日历推导:每个交易日固定 240 根(09:31-11:30, 13:01-15:00)。
func (p *MinutePortal) TradingMinutes(days []int64) []int64 {
	out := make([]int64, 0, len(days)*len(minuteSlots))
	for _, day := range days {
		base := day * 86400
		for _, slot := range minuteSlots {
			out = append(out, (base+int64(slot))*1_000_000)
		}
	}
	return out
}

// MicrosToDay 把 micros 转换为 epoch days。
func MicrosToDay(micros int64) int64 { return micros / (86400 * 1_000_000) }

// MicrosToTime 把 micros 转换为 UTC 时间。
func MicrosToTime(micros int64) time.Time { return time.UnixMicro(micros).UTC() }

// yuanFromThousand 把分钟成交额从千元换算为元。
//
// 分钟源数据在迁移时按 ÷1000 归一化为千元(原始值为整数元),乘回 1000 会产生
// 浮点噪声(如 16502.079×1000 = 16502079.000000002),这里贴近整数时吸附。
func yuanFromThousand(v float64) float64 {
	scaled := v * 1000
	if r := math.Round(scaled); math.Abs(scaled-r) <= 1e-6 {
		return r
	}
	return scaled
}

// calendarDays 返回 [lo, hi] 内的交易日;未注入日历时退化为自然日。
func (p *MinutePortal) calendarDays(lo, hi int64) []int64 {
	if p.calendar != nil {
		return p.calendar(lo, hi)
	}
	out := make([]int64, 0, hi-lo+1)
	for d := lo; d <= hi; d++ {
		out = append(out, d)
	}
	return out
}

func dedupeInt64(days []int64) []int64 {
	out := days[:0]
	var last int64
	for i, d := range days {
		if i > 0 && d == last {
			continue
		}
		out = append(out, d)
		last = d
	}
	return out
}

// isTimeOfDay 判断 micros 的日内时刻是否等于 hh:mm。
func isTimeOfDay(micros int64, hh, mm int) bool {
	sec := micros / 1_000_000 % 86400
	return sec == int64(hh*3600+mm*60)
}

// foldAuction 折叠开盘集合竞价:把 09:30 Bar 并入同日 09:31 Bar(与 quantbt 的 _fold_opening_auction 一致)。
//
// 规则:09:31 的开盘价取 09:30 的开盘价,最高/最低取两者极值,量额相加;09:30 Bar 不再可见。
func foldAuction(bars []MinuteBar) []MinuteBar {
	out := make([]MinuteBar, 0, len(bars))
	for i := 0; i < len(bars); i++ {
		cur := bars[i]
		if !isTimeOfDay(cur.Micros, 9, 30) {
			out = append(out, cur)
			continue
		}
		// 09:30:与紧随其后的 09:31 合并;没有则直接丢弃
		if i+1 < len(bars) && isTimeOfDay(bars[i+1].Micros, 9, 31) && MicrosToDay(bars[i+1].Micros) == MicrosToDay(cur.Micros) {
			next := bars[i+1]
			next.Open = cur.Open
			if cur.High > next.High {
				next.High = cur.High
			}
			if cur.Low < next.Low {
				next.Low = cur.Low
			}
			next.Volume += cur.Volume
			next.Amount += cur.Amount
			out = append(out, next)
			i++
		}
	}
	return out
}

// fillSuspensions 在已加载窗口内补齐停牌分钟(与 quantbt 的 _fill_minute_suspensions 一致):
// 缺失分钟用前收盘价填充,成交量 0,成交额 NaN。
//
// 起点为该证券在窗口内的首根 Bar(未上市/未交易前不填充),终点为窗口末尾
// (quantbt 一次性加载全区间,因此停牌会一直填充到恢复交易或回测结束)。
func (p *MinutePortal) fillSuspensions(sec string, bars []MinuteBar) []MinuteBar {
	if len(p.loaded) == 0 {
		return bars
	}
	windowStart := (p.loaded[0]*86400 + int64(minuteSlots[0])) * 1_000_000
	windowEnd := (p.loaded[len(p.loaded)-1]*86400 + int64(minuteSlots[len(minuteSlots)-1])) * 1_000_000
	start, end := windowStart, windowEnd
	if len(bars) > 0 {
		start = bars[0].Micros
	}
	prevClose := math.NaN()
	if len(bars) == 0 {
		// 窗口内无任何分钟数据(长期停牌):用最近有效日收盘价做种子
		if p.prevClose == nil {
			return bars
		}
		prevClose = p.prevClose(sec, MicrosToDay(windowStart))
		if math.IsNaN(prevClose) || prevClose <= 0 {
			return bars
		}
	}
	// 预分配精确容量:窗口内 [start,end] 的分钟数(避免 append 反复扩容)
	capacity := 0
	for _, day := range p.loaded {
		base := day * 86400
		for _, slot := range minuteSlots {
			micros := (base + int64(slot)) * 1_000_000
			if micros >= start && micros <= end {
				capacity++
			}
		}
	}
	if capacity < len(bars) {
		capacity = len(bars)
	}
	out := make([]MinuteBar, 0, capacity)
	i := 0
	for _, day := range p.loaded {
		base := day * 86400
		for _, slot := range minuteSlots {
			micros := (base + int64(slot)) * 1_000_000
			if micros < start || micros > end {
				continue
			}
			if i < len(bars) && bars[i].Micros == micros {
				prevClose = bars[i].Close
				out = append(out, bars[i])
				i++
				continue
			}
			out = append(out, MinuteBar{
				Micros: micros, Open: prevClose, High: prevClose, Low: prevClose,
				Close: prevClose, Volume: 0, Amount: math.NaN(),
			})
		}
	}
	for ; i < len(bars); i++ {
		out = append(out, bars[i])
	}
	return out
}

var _ = lake.PartitionPath
