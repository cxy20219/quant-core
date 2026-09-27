package btworker

import (
	"math"

	"quant-core/internal/btengine"
	"quant-core/internal/schema"
)

// hostView 是 worker 对引擎状态的受限视图(RPC 处理器使用)。
//
// 数据流:
//   - 引擎 → 视图:currentDay / previousDay / currentBars 由 sync() 拉取;
//   - 视图 → 引擎:股票池(universe)写回 host.Universe,引擎按它取数与构建 Bar 快照。
type hostView struct {
	host        *btengine.Host
	portal      *btengine.DataPortal
	broker      *btengine.Broker
	currentDay  int64
	previousDay int64
	currentBars map[string]btengine.Bar
}

func newHostView(host *btengine.Host) *hostView {
	return &hostView{
		host:        host,
		portal:      host.Portal,
		broker:      host.Broker,
		currentDay:  host.CurrentDay,
		previousDay: host.PreviousDay,
		currentBars: host.CurrentBars,
	}
}

// sync 把引擎最新状态同步进视图(每次生命周期回调前后调用)。
func (v *hostView) sync() {
	v.currentDay = v.host.CurrentDay
	v.previousDay = v.host.PreviousDay
	v.currentBars = v.host.CurrentBars
}

// universe 返回当前股票池(权威存放在引擎侧)。
func (v *hostView) getUniverse() []string { return v.host.Universe }

// setUniverse 设置股票池并写回引擎。
func (v *hostView) setUniverse(list []string) {
	v.host.Universe = list
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
func (v *hostView) portfolioJSON() map[string]any {
	v.sync()
	pf := v.host.Portfolio
	positions := map[string]any{}
	for sec, pos := range pf.Positions {
		positions[sec] = positionJSON(pos)
	}
	return map[string]any{
		"cash":            pf.Cash,
		"positions_value": pf.PositionsValue,
		"portfolio_value": pf.PortfolioValue,
		"returns":         pf.Returns,
		"positions":       positions,
	}
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
