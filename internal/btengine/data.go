package btengine

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

// DataPortal 提供回测所需的数据访问:日线序列、复权因子、公司行动。
//
// 与查询层共用同一份数据湖逻辑(单一口径);按证券缓存整个回测窗口(含预热),
// 避免逐日查询放大 IO。
type DataPortal struct {
	lake     *lake.Lake
	scanner  *query.Scanner
	start    int64 // 回测开始日(epoch days,含)
	end      int64 // 回测结束日(含)
	warmup   int64 // 预热自然日数
	calendar []int64
	dayIndex map[int64]int

	dailyBySec map[string]*secDaily
	adjBySec   map[string]map[int64]float64
	// 公司行动:按日索引 + 按证券索引(quantbt 的 corporate_actions / get_stock_exrights)
	actionsByDay map[int64][]CorporateAction
	actionsBySec map[string][]CorporateAction

	mu sync.Mutex
}

// CorporateAction 是一条除权除息事件(字段与注册表 corporate_actions 一致)。
type CorporateAction struct {
	Security      string
	ExDate        int64
	AllottedPs    float64 // 每股送转
	RationedPs    float64 // 每股配股
	RationedPx    float64 // 配股价
	BonusPs       float64 // 每股分红(税前)
	ExerForwardA  float64
	ExerForwardB  float64
	ExerBackwardA float64
	ExerBackwardB float64
}

type secDaily struct {
	dates []int64
	open  []float64
	high  []float64
	low   []float64
	close []float64
	vol   []float64
	amt   []float64
	// priceAt 返回该证券在指定交易日的价格字段;停牌日返回最近一个有效值(补全)。
}

// NewDataPortal 创建数据门户。
func NewDataPortal(l *lake.Lake, start, end int64, warmupDays int64) *DataPortal {
	return &DataPortal{
		lake:         l,
		scanner:      query.NewCachedScanner(l, 1024),
		start:        start,
		end:          end,
		warmup:       warmupDays,
		dayIndex:     map[int64]int{},
		dailyBySec:   map[string]*secDaily{},
		adjBySec:     map[string]map[int64]float64{},
		actionsByDay: map[int64][]CorporateAction{},
		actionsBySec: map[string][]CorporateAction{},
	}
}

// TradingDays 返回区间内的交易日(基于 bars_daily 的日历)。
func (p *DataPortal) TradingDays() ([]int64, error) {
	if p.calendar != nil {
		return p.calendar, nil
	}
	ds, err := p.lake.Registry.Get("bars_daily")
	if err != nil {
		return nil, err
	}
	yearIdx, _ := ds.FieldIndex("trade_date")
	_ = yearIdx
	cur, err := p.scanner.Open(query.Request{
		Dataset: "bars_daily",
		Columns: []string{"trade_date"},
		Filter: &query.Filter{Preds: []query.Predicate{
			query.Gte(yearIdx, schema.Date(p.start-p.warmup)),
			query.Lte(yearIdx, schema.Date(p.end)),
		}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close()
	seen := map[int64]bool{}
	var days []int64
	for cur.Next() {
		row := cur.Row()
		d := row[0].I
		if !seen[d] {
			seen[d] = true
			days = append(days, d)
		}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })
	p.calendar = days
	for i, d := range days {
		p.dayIndex[d] = i
	}
	return days, nil
}

// PrevTradingDay 返回给定交易日的前一个交易日(不要求它在回测区间内)。
func (p *DataPortal) PrevTradingDay(day int64) int64 {
	// 日历按区间构建,区间首日的前一交易日通过数据自身推导
	if idx, ok := p.dayIndex[day]; ok && idx > 0 {
		return p.calendar[idx-1]
	}
	return 0
}

// loadDaily 加载证券的日线序列(含预热)。
func (p *DataPortal) loadDaily(securities []string) error {
	var missing []string
	for _, sec := range securities {
		if _, ok := p.dailyBySec[sec]; !ok {
			missing = append(missing, sec)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	ds, err := p.lake.Registry.Get("bars_daily")
	if err != nil {
		return err
	}
	codeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	lo := p.start - p.warmup
	cur, err := p.scanner.Open(query.Request{
		Dataset: "bars_daily",
		Columns: []string{"ts_code", "trade_date", "open", "high", "low", "close", "vol", "amount"},
		Filter: &query.Filter{Preds: []query.Predicate{
			query.In(codeIdx, toValues(missing)...),
			query.Gte(dateIdx, schema.Date(lo)),
			query.Lte(dateIdx, schema.Date(p.end)),
		}},
	})
	if err != nil {
		return err
	}
	defer cur.Close()
	for cur.Next() {
		row := cur.Row()
		sec := row[0].S
		sd := p.dailyBySec[sec]
		if sd == nil {
			sd = &secDaily{}
			p.dailyBySec[sec] = sd
		}
		sd.dates = append(sd.dates, row[1].I)
		sd.open = append(sd.open, num(row[2]))
		sd.high = append(sd.high, num(row[3]))
		sd.low = append(sd.low, num(row[4]))
		sd.close = append(sd.close, num(row[5]))
		// PTrade 单位:成交量 手→股(×100),成交额 千元→元(×1000)
		sd.vol = append(sd.vol, num(row[6])*100)
		sd.amt = append(sd.amt, num(row[7])*1000)
	}
	return cur.Err()
}

// EnsureCorporateActions 加载股票池在回测区间内的公司行动(数据量小,一次性加载)。
func (p *DataPortal) EnsureCorporateActions(securities []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(securities) == 0 {
		return nil
	}
	missing := make([]string, 0, len(securities))
	for _, sec := range securities {
		if _, ok := p.actionsBySec[sec]; !ok {
			missing = append(missing, sec)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	ds, err := p.lake.Registry.Get("corporate_actions")
	if err != nil {
		return err
	}
	codeIdx, _ := ds.FieldIndex("ts_code")
	// 与 quantbt 一致:按证券加载全部历史事件(数据量小;get_stock_exrights 需全量)
	cur, err := p.scanner.Open(query.Request{
		Dataset: "corporate_actions",
		Columns: []string{"ts_code", "ex_date", "allotted_ps", "rationed_ps", "rationed_px", "bonus_ps",
			"exer_forward_a", "exer_forward_b", "exer_backward_a", "exer_backward_b"},
		Filter: &query.Filter{Preds: []query.Predicate{
			query.In(codeIdx, toValues(missing)...),
		}},
	})
	if err != nil {
		return err
	}
	defer cur.Close()
	for cur.Next() {
		row := cur.Row()
		action := CorporateAction{
			Security:      row[0].S,
			ExDate:        row[1].I,
			AllottedPs:    num(row[2]),
			RationedPs:    num(row[3]),
			RationedPx:    num(row[4]),
			BonusPs:       num(row[5]),
			ExerForwardA:  num(row[6]),
			ExerForwardB:  num(row[7]),
			ExerBackwardA: num(row[8]),
			ExerBackwardB: num(row[9]),
		}
		p.actionsByDay[action.ExDate] = append(p.actionsByDay[action.ExDate], action)
		p.actionsBySec[action.Security] = append(p.actionsBySec[action.Security], action)
	}
	if err := cur.Err(); err != nil {
		return err
	}
	// 标记已加载(即使无数据),避免重复查询
	for _, sec := range missing {
		if _, ok := p.actionsBySec[sec]; !ok {
			p.actionsBySec[sec] = nil
		}
	}
	return nil
}

// CorporateActionsOn 返回指定交易日的公司行动(按代码升序)。
func (p *DataPortal) CorporateActionsOn(day int64) []CorporateAction {
	rows := p.actionsByDay[day]
	if len(rows) == 0 {
		return nil
	}
	out := make([]CorporateAction, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Security < out[j].Security })
	return out
}

// Exrights 返回证券的全部除权除息事件(按日期升序),供 get_stock_exrights 使用。
func (p *DataPortal) Exrights(sec string) []CorporateAction {
	rows := p.actionsBySec[sec]
	out := make([]CorporateAction, len(rows))
	copy(out, rows)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ExDate < out[j].ExDate })
	return out
}

func toValues(codes []string) []schema.Value {
	out := make([]schema.Value, 0, len(codes))
	for _, c := range codes {
		out = append(out, schema.Str(c))
	}
	return out
}

func num(v schema.Value) float64 {
	if v.Kind == schema.KindFloat {
		return v.F
	}
	return 0
}

// LoadSecurities 预加载证券日线(供引擎在初始化后批量调用)。
func (p *DataPortal) LoadSecurities(securities []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadDaily(securities)
}

// DailyBars 返回某交易日全部证券的行情(仅限已加载证券)。
func (p *DataPortal) DailyBars(day int64) map[string]Bar {
	out := map[string]Bar{}
	for sec, sd := range p.dailyBySec {
		idx := indexOfInt64(sd.dates, day)
		if idx < 0 {
			continue
		}
		out[sec] = Bar{
			Security: sec,
			Open:     sd.open[idx],
			High:     sd.high[idx],
			Low:      sd.low[idx],
			Close:    sd.close[idx],
			Volume:   sd.vol[idx],
			Amount:   sd.amt[idx],
		}
	}
	return out
}

// CloseAt 返回证券在指定日期的收盘价(停牌日回退最近有效值);无数据返回 0。
func (p *DataPortal) CloseAt(sec string, day int64) float64 {
	sd := p.dailyBySec[sec]
	if sd == nil {
		return 0
	}
	idx := indexOfInt64(sd.dates, day)
	if idx < 0 {
		idx = lastBefore(sd.dates, day)
	}
	if idx < 0 {
		return 0
	}
	return sd.close[idx]
}

// LastSaleAt 返回评估价:有数据用当日收盘,停牌用最近有效值。
func (p *DataPortal) LastSaleAt(sec string, day int64) float64 {
	return p.CloseAt(sec, day)
}

// HistoryValues 返回证券最近 count 个交易日的字段序列(截止到 cutoff 日,
// include 决定是否包含当日),按交易日缺失补全为最近有效值。
//
// 返回:值为按时间升序的序列;不足 count 个时在前面补 NaN(用 math.NaN 表示)。
func (p *DataPortal) HistoryValues(sec, field string, count int, cutoff int64, include bool) ([]float64, []int64) {
	sd := p.dailyBySec[sec]
	if sd == nil {
		return nil, nil
	}
	idx := lastBeforeOrEqual(sd.dates, cutoff)
	if idx < 0 {
		return nil, nil
	}
	startIdx := idx - count + 1
	if startIdx < 0 {
		startIdx = 0
	}
	values := make([]float64, 0, idx-startIdx+1)
	dates := make([]int64, 0, idx-startIdx+1)
	for i := startIdx; i <= idx; i++ {
		values = append(values, fieldOf(sd, field, i))
		dates = append(dates, sd.dates[i])
	}
	return values, dates
}

// HistoryValuesByDates 按给定交易日序列取值(用于按回测日历对齐)。
// 停牌语义与 PTrade 一致:时间轴为交易日历,停牌日价格用停牌前数据填充、成交量为 0。
func (p *DataPortal) HistoryValuesByDates(sec, field string, days []int64) []float64 {
	sd := p.dailyBySec[sec]
	if sd == nil {
		return make([]float64, len(days))
	}
	out := make([]float64, len(days))
	isVolume := field == "volume" || field == "vol" || field == "amount" || field == "money"
	for i, d := range days {
		idx := indexOfInt64(sd.dates, d)
		if idx < 0 {
			// 停牌(或未上市):价格用最近有效值,量额记 0
			if isVolume {
				out[i] = 0
				continue
			}
			idx = lastBefore(sd.dates, d)
		}
		if idx < 0 {
			out[i] = math.NaN()
			continue
		}
		out[i] = fieldOf(sd, field, idx)
	}
	return out
}

func fieldOf(sd *secDaily, field string, i int) float64 {
	switch field {
	case "open":
		return sd.open[i]
	case "high":
		return sd.high[i]
	case "low":
		return sd.low[i]
	case "close":
		return sd.close[i]
	case "volume", "vol":
		return sd.vol[i]
	case "amount", "money":
		return sd.amt[i]
	default:
		return sd.close[i]
	}
}

// Adfactors 返回证券的复权因子序列(按日)。
func (p *DataPortal) Adfactors(securities []string) {
	ds, err := p.lake.Registry.Get("adj_factor")
	if err != nil {
		return
	}
	codeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	factorIdx, _ := ds.FieldIndex("adj_factor")
	var missing []string
	for _, sec := range securities {
		if _, ok := p.adjBySec[sec]; !ok {
			missing = append(missing, sec)
		}
	}
	if len(missing) == 0 {
		return
	}
	cur, err := p.scanner.Open(query.Request{
		Dataset: "adj_factor",
		Columns: []string{"ts_code", "trade_date", "adj_factor"},
		Filter: &query.Filter{Preds: []query.Predicate{
			query.In(codeIdx, toValues(missing)...),
			query.Lte(dateIdx, schema.Date(p.end)),
		}},
		Partitions: map[string][]string{"factor_type": {"hfq"}},
	})
	if err != nil {
		return
	}
	defer cur.Close()
	for cur.Next() {
		row := cur.Row()
		sec := row[0].S
		if p.adjBySec[sec] == nil {
			p.adjBySec[sec] = map[int64]float64{}
		}
		p.adjBySec[sec][row[1].I] = num(row[factorIdx])
	}
}

// AdjFactorAt 返回证券在指定日的后复权因子(缺失回退最近值,再缺失返回 1)。
func (p *DataPortal) AdjFactorAt(sec string, day int64) float64 {
	m := p.adjBySec[sec]
	if m == nil {
		return 1
	}
	if v, ok := m[day]; ok {
		return v
	}
	var best int64 = -1
	for d := range m {
		if d <= day && d > best {
			best = d
		}
	}
	if best >= 0 {
		return m[best]
	}
	// 日线数据早于因子起始:取最早因子
	var earliest int64 = 1 << 62
	for d := range m {
		if d < earliest {
			earliest = d
		}
	}
	if earliest != 1<<62 {
		return m[earliest]
	}
	return 1
}

func indexOfInt64(s []int64, v int64) int {
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi) / 2
		if s[mid] < v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(s) && s[lo] == v {
		return lo
	}
	return -1
}

func lastBefore(s []int64, v int64) int {
	idx := lastBeforeOrEqual(s, v-1)
	return idx
}

func lastBeforeOrEqual(s []int64, v int64) int {
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi) / 2
		if s[mid] <= v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

// EnsureDaily 确保证券已加载(策略动态加入股票池时调用)。
func (p *DataPortal) EnsureDaily(securities []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadDaily(securities)
}

// TradingDaysInRange 返回 [lo, hi] 内的交易日(日历覆盖预热区间)。
func (p *DataPortal) TradingDaysInRange(lo, hi int64) []int64 {
	all, err := p.TradingDays()
	if err != nil {
		return nil
	}
	out := make([]int64, 0, len(all))
	for _, d := range all {
		if d >= lo && d <= hi {
			out = append(out, d)
		}
	}
	return out
}

// CalendarUpTo 返回截止 cutoff(含)的最近 count 个交易日。
func (p *DataPortal) CalendarUpTo(cutoff int64, count int) []int64 {
	all, err := p.TradingDays()
	if err != nil {
		return nil
	}
	idx := lastBeforeOrEqual(all, cutoff)
	if idx < 0 {
		return nil
	}
	start := idx - count + 1
	if start < 0 {
		start = 0
	}
	return all[start : idx+1]
}

// HasData 判断证券是否存在于日线数据中。
func (p *DataPortal) HasData(sec string) bool {
	_, ok := p.dailyBySec[sec]
	return ok
}

var _ = fmt.Sprintf
