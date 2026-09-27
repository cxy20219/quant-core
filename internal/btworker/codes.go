package btworker

import "strings"

// PTrade 代码规范与内部规范互转(与 quantbt 的 codes.py 一致):
//
//	PTrade 风格(策略可见): 600570.SS / 000001.SZ / 830799.JY
//	内部风格(数据湖):     600570.SH / 000001.SZ / 830799.BJ
func toInternalCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	switch {
	case strings.HasSuffix(code, ".SS"):
		return code[:len(code)-3] + ".SH"
	case strings.HasSuffix(code, ".JY"):
		return code[:len(code)-3] + ".BJ"
	default:
		return code
	}
}

// toPTradeCode 内部码 → PTrade 码。
func toPTradeCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	switch {
	case strings.HasSuffix(code, ".SH"):
		return code[:len(code)-3] + ".SS"
	case strings.HasSuffix(code, ".BJ"):
		return code[:len(code)-3] + ".JY"
	default:
		return code
	}
}

// toInternalList 批量转换。
func toInternalList(codes []string) []string {
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		out = append(out, toInternalCode(code))
	}
	return out
}

// toPTradeList 批量转换。
func toPTradeList(codes []string) []string {
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		out = append(out, toPTradeCode(code))
	}
	return out
}
// toOrderSymbol 返回 PTrade 委托代码格式(.XSHG/.XSHE);与 quantbt 的 to_order_symbol 一致。
func toOrderSymbol(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	switch {
	case strings.HasSuffix(code, ".SH"):
		return code[:len(code)-3] + ".XSHG"
	case strings.HasSuffix(code, ".SZ"):
		return code[:len(code)-3] + ".XSHE"
	default:
		return code
	}
}