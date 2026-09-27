package btworker

import (
	"math"
	"strings"

	"quant-core/internal/btengine"
	"quant-core/internal/schema"
)

// hostView 是 worker 对引擎状态的受限视图(RPC 处理器使用)。
//
// 数据流:
//   - 引擎 → 视图:currentDay / previousDay / currentBars 由 sync() 拉取;
//   - 视图 → 引擎:股票池(universe)写回 host.Universe,引擎按它取数与构建 Bar 快照。
//
// 代码规范:与 quantbt 一致,策略侧看到的代码保留其"原始写法"
// (策略用 .SS 就显示 .SS,用 .SH 就显示 .SH);委托与成交统一为 .XSHG/.XSHE。
type hostView struct {
	host        *btengine.Host
	portal      *btengine.DataPortal
	broker      *btengine.Broker
	currentDay  int64
	previousDay int64
	currentBars map[string]btengine.Bar
	rawCodes    map[string]string // 内部码 → 策略原始码
}

func newHostView(host *btengine.Host) *hostView {
	return &hostView{
		host:        host,
		portal:      host.Portal,
		broker:      host.Broker,
		currentDay:  host.CurrentDay,
		previousDay: host.PreviousDay,
		currentBars: host.CurrentBars,
		rawCodes:    map[string]string{},
	}
}

// display 返回策略侧应看到的代码(优先原始写法,否则转为 PTrade 风格)。
func (v *hostView) display(internal string) string {
	if raw, ok := v.rawCodes[internal]; ok {
		return raw
	}
	return toPTradeCode(internal)
}

// sync 把引擎最新状态同步进视图(每次生命周期回调前后调用)。
func (v *hostView) sync() {
	v.currentDay = v.host.CurrentDay
	v.previousDay = v.host.PreviousDay
	v.currentBars = v.host.CurrentBars
}

// universe 返回当前股票池(权威存放在引擎侧)。
func (v *hostView) getUniverse() []string { return v.host.Universe }

// setUniverse 设置股票池并写回引擎,同时记录策略侧的原始代码写法。
func (v *hostView) setUniverse(raw []string) {
	internal := make([]string, 0, len(raw))
	v.rawCodes = make(map[string]string, len(raw))
	for _, code := range raw {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" {
			continue
		}
		key := toInternalCode(code)
		internal = append(internal, key)
		v.rawCodes[key] = code
	}
	v.host.Universe = internal
}

// clockText 返回当前时钟文本(YYYY-MM-DD HH:MM:SS)。
func (v *hostView) clockText() string {
	return v.host.Engine.BrokerClockText()
}

// clockHM 返回当前时钟的 HH:MM(用于 run_daily 触发判定;日线模式为空)。
func (v *hostView) clockHM() string {
	if v.host.Engine.Config.Frequency != "1m" {
		return ""
	}
	text := v.host.Engine.BrokerClockText()
	if len(text) >= 16 {
		return text[11:16]
	}
	return ""
}

// currentMicros 返回当前模拟时刻的 micros。
func (v *hostView) currentMicros() int64 {
	return v.host.Engine.BrokerCurrentMicros()
}

func (v *hostView) dayString() string {
	v.sync()
	if v.currentDay == 0 {
		return ""
	}
	return schema.FormatDateISO(v.currentDay)
}

func (v *hostView) previousDayString() string {
	v.sync()
	if v.previousDay == 0 {
		return ""
	}
	return schema.FormatDateISO(v.previousDay)
}

// portfolioJSON 返回账户快照(传给 Python 的 context.portfolio)。
//
// 与 PTrade/quantbt 一致:调用时必须按最新持仓价重算市值,这样策略在
// 下单回调内读到的 cash/portfolio_value 就是下单后的即时状态。
func (v *hostView) portfolioJSON() map[string]any {
	v.sync()
	pf := v.host.Portfolio
	positions := map[string]any{}
	for sec, pos := range pf.Positions {
		positions[v.display(sec)] = positionJSONWith(pos, v.display)
	}
	// 与 quantbt 一致:portfolio_value 只在 mark_to_market / 即时成交后刷新,
	// 这里返回已存储的值(不在快照时重算)。
	return map[string]any{
		"cash":            pf.Cash,
		"positions_value": pf.PositionsValue,
		"portfolio_value": pf.PortfolioValue,
		"returns":         pf.Returns,
		"positions":       positions,
	}
}

// displayList 批量转换内部码为策略侧代码。
func (v *hostView) displayList(codes []string) []string {
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		out = append(out, v.display(code))
	}
	return out
}

// displayKeyedData 把以内部码为键的数据字典转为策略侧代码键。
func (v *hostView) displayKeyedData(data map[string]map[string][]any) map[string]map[string][]any {
	out := make(map[string]map[string][]any, len(data))
	for code, fields := range data {
		out[v.display(code)] = fields
	}
	return out
}

func (v *hostView) appendLog(level, message string) {
	ts := ""
	if v.currentDay != 0 {
		ts = schema.FormatDateISO(v.currentDay) + " 15:00:00"
	}
	*v.host.Logs = append(*v.host.Logs, btengine.LogRecord{
		Timestamp: ts,
		Level:     level,
		Message:   message,
	})
}

// calendarUpTo 返回截止 cutoff 的最近 count 个交易日。
func (v *hostView) calendarUpTo(cutoff int64, count int) []int64 {
	if cutoff == 0 {
		v.sync()
		cutoff = v.currentDay
	}
	return v.portal.CalendarUpTo(cutoff, count)
}

// valuesFor 返回证券在给定交易日序列上的字段值,按需复权。
func (v *hostView) valuesFor(sec, field string, days []int64, fq string, cutoff int64) []any {
	out := make([]any, len(days))
	if len(days) == 0 {
		return out
	}
	base := v.portal.HistoryValuesByDates(sec, field, days)
	needAdj := fq == "post" || fq == "pre" || fq == "q" || fq == "hfq" || fq == "qfq"
	normalize := fq == "pre" || fq == "qfq"
	norm := 1.0
	if normalize {
		norm = v.portal.AdjFactorAt(sec, cutoff)
		if norm == 0 {
			norm = 1
		}
	}
	for i, d := range days {
		val := base[i]
		if math.IsNaN(val) {
			out[i] = nil
			continue
		}
		if needAdj {
			factor := v.portal.AdjFactorAt(sec, d)
			val = val * factor
			if normalize {
				val = val / norm
			}
		}
		out[i] = val
	}
	return out
}
