package btengine

import (
	"fmt"
	"sort"
)

// HandlingFeeRate 是经手费率(与 quantbt 对齐)。
const HandlingFeeRate = 0.0000487

// Broker 负责订单、撮合、费用与持仓变动。
type Broker struct {
	portfolio    *Portfolio
	commission   Commission
	slippage     Slippage
	volumeRatio  float64
	limitMode    string // LIMIT / UNLIMITED
	orders       []*Order
	orderIndex   map[string]*Order
	pending      []*Order // 未成交限价单
	trades       []*Trade
	tradeSeq     int
	currentDay   int64
	currentCloses map[string]float64
	volumeUsed   map[string]float64 // 当 Bar 该证券已用成交量
}

// NewBroker 创建撮合器。
func NewBroker(p *Portfolio) *Broker {
	return &Broker{
		portfolio:     p,
		commission:    DefaultCommission(),
		volumeRatio:   0.25,
		limitMode:     "LIMIT",
		orderIndex:    map[string]*Order{},
		volumeUsed:    map[string]float64{},
	}
}

// SetCommission 设置费用。
func (b *Broker) SetCommission(c Commission) { b.commission = c }

// SetSlippage 设置滑点。
func (b *Broker) SetSlippage(s Slippage) { b.slippage = s }

// SetVolumeRatio 设置成交量比例。
func (b *Broker) SetVolumeRatio(r float64) { b.volumeRatio = r }

// SetLimitMode 设置限价模式。
func (b *Broker) SetLimitMode(m string) { b.limitMode = m }

// StartDay 开始新的交易日。
func (b *Broker) StartDay(day int64) {
	b.currentDay = day
	b.volumeUsed = map[string]float64{}
}

// SetCurrentPrices 记录当前 Bar 的收盘价(用于撮合与估值)。
func (b *Broker) SetCurrentPrices(bars map[string]Bar) {
	b.currentCloses = map[string]float64{}
	for sec, bar := range bars {
		b.currentCloses[sec] = bar.Close
	}
}

// Order 提交订单。amount 正买负卖;limitPrice<=0 表示市价。
func (b *Broker) Order(sec string, amount int64, limitPrice float64, bar Bar) *Order {
	amount = b.roundAmount(sec, amount)
	if amount == 0 {
		return nil
	}
	order := &Order{
		ID:         b.nextOrderID(),
		Security:   sec,
		Amount:     amount,
		OrigAmount: amount,
		Limit:      limitPrice,
		Status:     "2",
		CreatedAt:  dayString(b.currentDay),
	}
	if limitPrice <= 0 {
		order.OrderType = "market"
		b.fill(order, amount, b.executionPrice(bar.Close, amount), bar)
	} else {
		order.OrderType = "limit"
		if b.marketable(amount, limitPrice, bar.Close) {
			b.fill(order, amount, b.executionPrice(limitPrice, amount), bar)
		} else {
			b.pending = append(b.pending, order)
		}
	}
	b.orders = append(b.orders, order)
	b.orderIndex[order.ID] = order
	return order
}

// MatchPending 撮合挂单(每个 Bar 结束后调用)。
func (b *Broker) MatchPending(bars map[string]Bar) {
	remaining := b.pending[:0]
	for _, order := range b.pending {
		bar, ok := bars[order.Security]
		if !ok {
			remaining = append(remaining, order) // 停牌或无行情,继续挂单
			continue
		}
		if !b.marketable(order.Amount, order.Limit, bar.Close) {
			remaining = append(remaining, order)
			continue
		}
		b.fill(order, order.Amount-order.Filled, b.executionPrice(order.Limit, order.Amount), bar)
	}
	b.pending = remaining
}

// ExpirePending 当日结束使未成交挂单失效(与 PTrade 一致:日级挂单在
// after_trading_end 后撤销;此处保留订单记录并标记状态 9 以便审计)。
func (b *Broker) ExpirePending() {
	for _, o := range b.pending {
		o.Status = "9"
	}
	b.pending = nil
}

// Positions 返回持仓表。
func (b *Broker) Positions() map[string]*Position { return b.portfolio.Positions }

// Orders 返回全部委托。
func (b *Broker) Orders() []*Order { return b.orders }

// OpenOrders 返回未完成委托。
func (b *Broker) OpenOrders() []*Order {
	out := make([]*Order, 0, len(b.pending))
	for _, o := range b.pending {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Trades 返回全部成交。
func (b *Broker) Trades() []*Trade { return b.trades }

// Cancel 撤销挂单;返回是否成功。
func (b *Broker) Cancel(orderID string) bool {
	for i, o := range b.pending {
		if o.ID == orderID {
			o.Status = "9" // 已撤
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			return true
		}
	}
	return false
}

// GetOrder 按 id 查委托。
func (b *Broker) GetOrder(id string) *Order { return b.orderIndex[id] }

// GetPosition 查持仓(只读;不存在时返回空持仓,不写入账户)。
func (b *Broker) GetPosition(sec string) *Position {
	if pos, ok := b.portfolio.Positions[sec]; ok {
		return pos
	}
	return &Position{Security: sec}
}

// ensurePosition 取持仓,不存在则创建并登记到账户。
func (b *Broker) ensurePosition(sec string) *Position {
	if pos, ok := b.portfolio.Positions[sec]; ok {
		return pos
	}
	pos := &Position{Security: sec}
	b.portfolio.Positions[sec] = pos
	return pos
}

// refillCash 回收卖出现金并扣费(内部使用)。
func (b *Broker) fill(order *Order, amount int64, price float64, bar Bar) {
	if amount == 0 {
		return
	}
	fillAmount := b.capVolume(order.Security, amount, bar.Volume)
	if fillAmount == 0 {
		return
	}
	value := float64(abs64(fillAmount)) * price
	fees := b.fees(value, fillAmount < 0)
	pos := b.ensurePosition(order.Security)
	if fillAmount > 0 {
		// 买入:现金需覆盖 成交额 + 费用;不足则按手数回退
		for fillAmount > 0 {
			value = float64(fillAmount) * price
			fees = b.fees(value, false)
			if value+fees <= b.portfolio.Cash {
				break
			}
			lot := int64(lotSize(order.Security))
			fillAmount -= lot
		}
		if fillAmount <= 0 {
			return
		}
	}
	if fillAmount > 0 {
		// 更新成本价(含费用)
		totalCost := pos.CostBasis*float64(pos.Amount) + value + fees
		pos.Amount += fillAmount
		pos.EnableAmount += fillAmount
		if pos.Amount > 0 {
			pos.CostBasis = totalCost / float64(pos.Amount)
		}
		b.portfolio.Cash -= value + fees
	} else {
		sellAmount := -fillAmount
		if sellAmount > pos.EnableAmount {
			sellAmount = pos.EnableAmount
		}
		if sellAmount <= 0 {
			return
		}
		fillAmount = -sellAmount
		value = float64(sellAmount) * price
		fees = b.fees(value, true)
		pos.Amount -= sellAmount
		pos.EnableAmount -= sellAmount
		b.portfolio.Cash += value - fees
		if pos.Amount == 0 {
			pos.CostBasis = 0
		}
	}
	order.Filled += abs64(fillAmount)
	order.FilledAmt = order.Filled
	order.FilledPx = price
	order.FilledAt = dayString(b.currentDay)
	if abs64(order.Filled) >= abs64(order.OrigAmount) {
		order.Status = "8"
	} else {
		order.Status = "6"
	}
	b.tradeSeq++
	trade := &Trade{
		TradeID:   fmt.Sprintf("T%06d", b.tradeSeq),
		OrderID:   order.ID,
		Security:  order.Security,
		Side:      map[bool]string{true: "buy", false: "sell"}[fillAmount > 0],
		Amount:    abs64(fillAmount),
		Price:     price,
		Value:     float64(abs64(fillAmount)) * price,
		TradeTime: dayString(b.currentDay) + " 15:00:00",
	}
	b.trades = append(b.trades, trade)
}

func (b *Broker) nextOrderID() string {
	return fmt.Sprintf("O%06d", len(b.orders)+1)
}

func (b *Broker) marketable(amount int64, limit, price float64) bool {
	if limit <= 0 {
		return true
	}
	if amount > 0 {
		return limit >= price
	}
	return limit <= price
}

func (b *Broker) executionPrice(price float64, amount int64) float64 {
	if b.slippage.Value == 0 {
		return price
	}
	dir := 1.0
	if amount < 0 {
		dir = -1
	}
	if b.slippage.Fixed {
		return price + dir*b.slippage.Value/2
	}
	return price * (1 + dir*b.slippage.Value/2)
}

func (b *Broker) fees(value float64, isSell bool) float64 {
	commission := value * b.commission.Cost
	if commission < b.commission.MinTradeCost {
		commission = b.commission.MinTradeCost
	}
	handling := value * HandlingFeeRate
	tax := 0.0
	if isSell {
		tax = value * b.commission.Tax
	}
	return commission + handling + tax
}

func (b *Broker) capVolume(sec string, amount int64, volume float64) int64 {
	if b.limitMode == "UNLIMITED" || amount == 0 || volume <= 0 {
		return amount
	}
	maxAmount := int64(volume * b.volumeRatio)
	maxAmount = int64(maxAmount / int64(lotSize(sec)) * int64(lotSize(sec)))
	used := b.volumeUsed[sec]
	remaining := float64(maxAmount) - used
	if remaining < 0 {
		remaining = 0
	}
	fill := int64(abs64(amount))
	if float64(fill) > remaining {
		fill = int64(remaining)
	}
	fill = int64(fill / int64(lotSize(sec)) * int64(lotSize(sec)))
	b.volumeUsed[sec] = used + float64(fill)
	if amount > 0 {
		return fill
	}
	if fill > abs64(amount) {
		fill = abs64(amount)
	}
	return -fill
}

func (b *Broker) roundAmount(sec string, amount int64) int64 {
	lot := int64(lotSize(sec))
	if amount > 0 {
		return amount / lot * lot
	}
	if amount < 0 {
		pos := b.GetPosition(sec)
		if abs64(amount) >= pos.Amount {
			return -pos.Amount
		}
		return -(abs64(amount) / lot * lot)
	}
	return 0
}

func lotSize(sec string) int {
	if len(sec) >= 2 {
		switch sec[:2] {
		case "11", "12", "13":
			return 10
		}
	}
	return 100
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func dayString(days int64) string {
	t := DaysToTime(days)
	return t.Format("2006-01-02")
}
