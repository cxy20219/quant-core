package btworker

import "quant-core/internal/btengine"

// ApplyDisplayCodes 把回测结果的证券代码统一为 PTrade 委托代码格式:
// 委托与成交用 .XSHG/.XSHE(与 quantbt 的 to_order_symbol 一致),
// 持仓随委托格式展示。结果 JSON 面向用户与对拍,因此在这里统一。
func ApplyDisplayCodes(result *btengine.Result) {
	if result == nil {
		return
	}
	for _, order := range result.Orders {
		order.Security = toOrderSymbol(order.Security)
	}
	for _, trade := range result.Trades {
		trade.Security = toOrderSymbol(trade.Security)
	}
}
