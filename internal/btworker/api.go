package btworker

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"quant-core/internal/btengine"
	"quant-core/internal/schema"
)

// StrategyRunner 实现(供 btengine.Engine 调用)。
func (w *Worker) Initialize(host *btengine.Host, cfg btengine.Config) error {
	w.host = newHostView(host)
	if err := w.start(); err != nil {
		return err
	}
	meta := map[string]any{
		"capital_base": cfg.CapitalBase,
		"start_date":   cfg.StartDate.Format("2006-01-02"),
		"end_date":     cfg.EndDate.Format("2006-01-02"),
		"frequency":    cfg.Frequency,
		"strategy_name": cfg.StrategyName,
	}
	params := cfg.Params
	if params == nil {
		params = map[string]any{}
	}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := w.send(&message{
		Type:        "init",
		StrategySrc: w.cfg.StrategySource,
		Params:      paramsRaw,
		Meta:        meta,
	}); err != nil {
		return err
	}
	return w.waitDone("initialize")
}

// BeforeTradingStart 触发 before_trading_start。
func (w *Worker) BeforeTradingStart() error {
	if err := w.send(&message{Type: "phase", Name: "before_trading_start", Day: w.host.dayString(), Portfolio: w.host.portfolioJSON()}); err != nil {
		return err
	}
	return w.waitDone("before_trading_start")
}

// HandleData 触发 handle_data。
func (w *Worker) HandleData(snapshot map[string]btengine.BarSnapshot) error {
	bars := make(map[string]map[string]any, len(snapshot))
	for code, bar := range snapshot {
		// 策略侧使用 PTrade 代码(.SS/.SZ/.JY)
		bars[w.host.display(code)] = map[string]any{
			"open": bar.Open, "high": bar.High, "low": bar.Low,
			"close": bar.Close, "volume": bar.Volume, "amount": bar.Amount,
		}
	}
	if err := w.send(&message{
		Type:        "bar",
		Day:         w.host.dayString(),
		PreviousDay: w.host.previousDayString(),
		Bars:        bars,
		Portfolio:   w.host.portfolioJSON(),
	}); err != nil {
		return err
	}
	return w.waitDone("handle_data")
}

// RunDaily 触发 run_daily 注册的回调。
func (w *Worker) RunDaily() error {
	if err := w.send(&message{Type: "phase", Name: "run_daily", Day: w.host.dayString(), Portfolio: w.host.portfolioJSON()}); err != nil {
		return err
	}
	return w.waitDone("run_daily")
}

// AfterTradingEnd 触发 after_trading_end。
func (w *Worker) AfterTradingEnd() error {
	if err := w.send(&message{Type: "phase", Name: "after_trading_end", Day: w.host.dayString()}); err != nil {
		return err
	}
	return w.waitDone("after_trading_end")
}

// Close 结束策略进程。
func (w *Worker) Close() error {
	if w.stdin == nil {
		return nil
	}
	_ = w.send(&message{Type: "shutdown"})
	_ = w.stdin.Close()
	if w.cmd != nil {
		_ = w.cmd.Wait()
	}
	return nil
}

// waitDone 读取消息直到策略回调结束,期间处理 RPC 调用与日志。
func (w *Worker) waitDone(phase string) error {
	for {
		msg, err := w.read()
		if err != nil {
			return fmt.Errorf("策略进程在 %s 阶段退出: %w", phase, err)
		}
		switch msg.Type {
		case "done":
			return nil
		case "ready":
			return nil
		case "log":
			w.host.appendLog(msg.Level, msg.Message)
		case "call":
			w.handleCall(msg)
		case "error":
			detail := msg.Message
			if msg.Traceback != "" {
				detail += "\n" + msg.Traceback
			}
			return fmt.Errorf("策略错误(%s): %s", phase, detail)
		default:
			// 未知消息忽略,避免阻塞
		}
	}
}

func (w *Worker) handleCall(msg *message) {
	result, err := w.dispatch(msg.Method, msg.Params)
	resp := &message{Type: "result", ID: msg.ID}
	if err != nil {
		resp.Error = err.Error()
	} else {
		raw, mErr := json.Marshal(result)
		if mErr != nil {
			resp.Error = mErr.Error()
		} else {
			resp.Result = raw
		}
	}
	_ = w.send(resp)
}

// ── RPC 方法实现 ─────────────────────────────────────────────────────────

func (w *Worker) dispatch(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "history":
		return w.rpcHistory(raw)
	case "price":
		return w.rpcPrice(raw)
	case "order", "order_target", "order_value", "order_target_value":
		return w.rpcOrder(method, raw)
	case "cancel_order":
		return w.rpcCancel(raw)
	case "portfolio":
		return w.host.portfolioJSON(), nil
	case "get_order", "get_orders", "get_open_orders", "get_trades",
		"get_position", "get_positions":
		return w.rpcQuery(method, raw)
	case "set_universe", "set_benchmark", "set_commission", "set_slippage",
		"set_fixed_slippage", "set_volume_ratio", "set_limit_mode":
		return w.rpcSetting(method, raw)
	case "get_stock_exrights":
		return nil, nil // v1:公司行动数据覆盖有限,返回空
	default:
		return nil, fmt.Errorf("不支持的策略 API: %s", method)
	}
}

type historyParams struct {
	Count        int      `json:"count"`
	Frequency    string   `json:"frequency"`
	Field        string   `json:"field"`
	SecurityList []string `json:"security_list"`
	FQ           *string  `json:"fq"`
	Include      bool     `json:"include"`
	Fill         string   `json:"fill"`
}

func (w *Worker) rpcHistory(raw json.RawMessage) (any, error) {
	var p historyParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	securities := []string{}
	for _, s := range p.SecurityList {
		securities = append(securities, toInternalCode(s))
	}
	if len(securities) == 0 {
		securities = w.host.getUniverse()
	}
	if len(securities) == 0 {
		return nil, fmt.Errorf("get_history: 未指定 security_list 且未设置股票池")
	}
	if p.Count <= 0 {
		p.Count = 1
	}
	field := p.Field
	if field == "" {
		field = "close"
	}
	// include=False:截止到当前交易日之前(首日则取数据中的上一交易日)
	cutoff := w.host.currentDay - 1
	if p.Include {
		cutoff = w.host.currentDay
	}
	calendar := w.host.calendarUpTo(cutoff, p.Count)
	fq := ""
	if p.FQ != nil {
		fq = strings.ToLower(*p.FQ)
	}
	data := map[string]map[string][]any{}
	for _, sec := range securities {
		values := w.host.valuesFor(sec, field, calendar, fq, cutoff)
		data[sec] = map[string][]any{field: values}
	}
	dates := make([]string, len(calendar))
	for i, d := range calendar {
		dates[i] = schema.FormatDateISO(d)
	}
	return map[string]any{
		"dates":      dates,
		"securities": w.host.displayList(securities),
		"fields":     []string{field},
		"data":       w.host.displayKeyedData(data),
	}, nil
}

func (w *Worker) rpcPrice(raw json.RawMessage) (any, error) {
	var p struct {
		Security  string   `json:"security"`
		StartDate string   `json:"start_date"`
		EndDate   string   `json:"end_date"`
		Frequency string   `json:"frequency"`
		Fields    []string `json:"fields"`
		FQ        *string  `json:"fq"`
		Count     *int     `json:"count"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Security == "" {
		return nil, fmt.Errorf("get_price: security 必填")
	}
	count := 1
	if p.Count != nil && *p.Count > 0 {
		count = *p.Count
	}
	cutoff := w.host.currentDay - 1
	if p.EndDate != "" {
		if days, err := schema.ParseDate(p.EndDate); err == nil && days <= w.host.currentDay {
			cutoff = days
		} else if err == nil {
			cutoff = w.host.currentDay
		}
	}
	calendar := w.host.calendarUpTo(cutoff, count)
	if p.StartDate != "" {
		if days, err := schema.ParseDate(p.StartDate); err == nil {
			calendar = filterFrom(calendar, days)
		}
	}
	fields := p.Fields
	if len(fields) == 0 {
		fields = []string{"close"}
	}
	fq := ""
	if p.FQ != nil {
		fq = strings.ToLower(*p.FQ)
	}
	data := map[string]map[string][]any{}
	for _, field := range fields {
		values := w.host.valuesFor(p.Security, field, calendar, fq, cutoff)
		if data[p.Security] == nil {
			data[p.Security] = map[string][]any{}
		}
		data[p.Security][field] = values
	}
	dates := make([]string, len(calendar))
	for i, d := range calendar {
		dates[i] = schema.FormatDateISO(d)
	}
	return map[string]any{
		"dates":      dates,
		"securities": []string{w.host.display(p.Security)},
		"fields":     fields,
		"data":       w.host.displayKeyedData(data),
	}, nil
}

type orderParams struct {
	Security   string   `json:"security"`
	Amount     *int64   `json:"amount"`
	Value      *float64 `json:"value"`
	LimitPrice *float64 `json:"limit_price"`
}

func (w *Worker) rpcOrder(method string, raw json.RawMessage) (any, error) {
	var p orderParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	p.Security = toInternalCode(p.Security)
	bar, ok := w.host.currentBars[p.Security]
	if !ok {
		// 与 PTrade 一致:无当前行情时不下单,返回 None
		return nil, nil
	}
	limit := 0.0
	if p.LimitPrice != nil {
		limit = *p.LimitPrice
	}
	var amount int64
	switch method {
	case "order", "order_target":
		if p.Amount == nil {
			return nil, fmt.Errorf("%s: amount 必填", method)
		}
		amount = *p.Amount
		if method == "order_target" {
			pos := w.host.broker.GetPosition(p.Security)
			amount = amount - pos.Amount
		}
	case "order_value":
		if p.Value == nil {
			return nil, fmt.Errorf("%s: value 必填", method)
		}
		// 与 quantbt 一致:int(value/price) 后由 Order 内部按手数取整
		amount = int64(*p.Value / bar.Close)
	case "order_target_value":
		if p.Value == nil {
			return nil, fmt.Errorf("%s: value 必填", method)
		}
		// 与 quantbt 一致:先把目标市值换算成手数并取整,再与当前持仓求差
		pos := w.host.broker.GetPosition(p.Security)
		targetAmount := int64(*p.Value / bar.Close)
		targetAmount = w.roundToLot(p.Security, targetAmount)
		amount = targetAmount - pos.Amount
	}
	order := w.host.broker.Order(p.Security, amount, limit, bar)
	if order == nil {
		return nil, nil
	}
	return orderJSONWith(order, w.host.display), nil
}

func (w *Worker) rpcCancel(raw json.RawMessage) (any, error) {
	var p struct {
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return w.host.broker.Cancel(p.OrderID), nil
}

func (w *Worker) rpcQuery(method string, raw json.RawMessage) (any, error) {
	var p struct {
		Security string `json:"security"`
		OrderID  string `json:"order_id"`
	}
	if raw != nil {
		_ = json.Unmarshal(raw, &p)
	}
	if p.Security != "" {
		p.Security = toInternalCode(p.Security)
	}
	switch method {
	case "get_order":
		order := w.host.broker.GetOrder(p.OrderID)
		if order == nil {
			return nil, nil
		}
		return orderJSONWith(order, w.host.display), nil
	case "get_orders":
		out := []map[string]any{}
		for _, o := range w.host.broker.Orders() {
			if p.Security != "" && o.Security != p.Security {
				continue
			}
			out = append(out, orderJSONWith(o, w.host.display))
		}
		return out, nil
	case "get_open_orders":
		out := []map[string]any{}
		for _, o := range w.host.broker.OpenOrders() {
			if p.Security != "" && o.Security != p.Security {
				continue
			}
			out = append(out, orderJSONWith(o, w.host.display))
		}
		return out, nil
	case "get_trades":
		out := map[string][][]any{}
		for _, tr := range w.host.broker.Trades() {
			side := "买"
			if tr.Side == "sell" {
				side = "卖"
			}
			out[tr.OrderID] = append(out[tr.OrderID], []any{
				tr.TradeID, tr.OrderID, toOrderSymbol(tr.Security), side,
				float64(tr.Amount), tr.Price, tr.Value, tr.TradeTime,
			})
		}
		return out, nil
	case "get_position":
		if p.Security == "" {
			return nil, fmt.Errorf("get_position: security 必填")
		}
		return positionJSONWith(w.host.broker.GetPosition(p.Security), w.host.display), nil
	case "get_positions":
		out := map[string]any{}
		if p.Security != "" {
			out[w.host.display(p.Security)] = positionJSONWith(w.host.broker.GetPosition(p.Security), w.host.display)
			return out, nil
		}
		for sec, pos := range w.host.broker.Positions() {
			out[w.host.display(sec)] = positionJSONWith(pos, w.host.display)
		}
		return out, nil
	}
	return nil, fmt.Errorf("未知查询: %s", method)
}

func (w *Worker) rpcSetting(method string, raw json.RawMessage) (any, error) {
	var p struct {
		Securities      []string `json:"securities"`
		CommissionRatio *float64 `json:"commission_ratio"`
		MinCommission   *float64 `json:"min_commission"`
		Slippage        *float64 `json:"slippage"`
		VolumeRatio     *float64 `json:"volume_ratio"`
		LimitMode       string   `json:"limit_mode"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	switch method {
	case "set_universe":
		w.host.setUniverse(p.Securities)
		if err := w.host.portal.EnsureDaily(w.host.getUniverse()); err != nil {
			return nil, err
		}
		w.host.portal.Adfactors(w.host.getUniverse())
	case "set_benchmark":
		// 基准仅用于业绩对比,引擎侧已通过配置设置;此处记录不做额外处理
	case "set_commission":
		c := w.host.broker
		current := btengine.DefaultCommission()
		if p.CommissionRatio != nil {
			current.Cost = *p.CommissionRatio
		}
		if p.MinCommission != nil {
			current.MinTradeCost = *p.MinCommission
		}
		c.SetCommission(current)
	case "set_slippage":
		if p.Slippage != nil {
			w.host.broker.SetSlippage(btengine.Slippage{Value: *p.Slippage})
		}
	case "set_fixed_slippage":
		if p.Slippage != nil {
			w.host.broker.SetSlippage(btengine.Slippage{Value: *p.Slippage, Fixed: true})
		}
	case "set_volume_ratio":
		if p.VolumeRatio != nil {
			w.host.broker.SetVolumeRatio(*p.VolumeRatio)
		}
	case "set_limit_mode":
		if p.LimitMode != "" {
			w.host.broker.SetLimitMode(strings.ToUpper(p.LimitMode))
		}
	}
	return nil, nil
}

// ── JSON 辅助 ────────────────────────────────────────────────────────────

func orderJSONWith(o *btengine.Order, display func(string) string) map[string]any {
	return map[string]any{
		"id":           o.ID,
		"security":     display(o.Security),
		"amount":       o.Origin(),
		"filled":       o.Filled,
		"limit_price":  o.Limit,
		"status":       o.Status,
		"order_type":   o.OrderType,
		"created_at":   o.CreatedAt,
		"filled_at":    o.FilledAt,
		"filled_price": o.FilledPx,
	}
}

func positionJSONWith(p *btengine.Position, display func(string) string) map[string]any {
	return map[string]any{
		"security":        display(p.Security),
		"amount":          p.Amount,
		"enable_amount":   p.EnableAmount,
		"last_sale_price": p.LastSalePrice,
		"cost_basis":      p.CostBasis,
		"today_amount":    p.TodayAmount,
	}
}

// toPTradeKeyedData 把以内部码为键的数据字典转为 PTrade 码键。
func toPTradeKeyedData(data map[string]map[string][]any) map[string]map[string][]any {
	out := make(map[string]map[string][]any, len(data))
	for code, fields := range data {
		out[toPTradeCode(code)] = fields
	}
	return out
}

// roundToLot 把数量向下取整到手数(与引擎的 _round_amount 一致)。
func (w *Worker) roundToLot(sec string, amount int64) int64 {
	lot := int64(100)
	if len(sec) >= 2 {
		switch sec[:2] {
		case "11", "12", "13":
			lot = 10
		}
	}
	if amount <= 0 {
		return 0
	}
	return amount / lot * lot
}

func normalizeUniverse(list []string) []string {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, code := range list {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return out
}

func filterFrom(days []int64, from int64) []int64 {
	out := make([]int64, 0, len(days))
	for _, d := range days {
		if d >= from {
			out = append(out, d)
		}
	}
	return out
}

var _ = math.Abs
