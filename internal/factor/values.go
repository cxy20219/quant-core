package factor

import (
	"path/filepath"
	"sync"
	"time"

	"quant-core/internal/lake"
)

// RegistryPath 返回湖内因子注册表路径。
func RegistryPath(l *lake.Lake) string {
	return filepath.Join(l.Root, "meta", "factors.json")
}

// LoadFor 读取湖内注册表(供回测/接口共用)。
func LoadFor(l *lake.Lake) (*Registry, error) {
	return LoadRegistry(RegistryPath(l))
}

// ── 单日截面值(带缓存,供回测策略高频调用)────────────────────────────

var (
	dailyMu    sync.Mutex
	dailyCache = map[string]map[string]float64{} // key: factorID|day
	dailyOrder []string
)

const dailyCacheMax = 256

// DailyValues 计算某因子在某交易日的截面值(code → value)。
//
// 基于 daily_cross 截面表(缺失年份回退 bars_daily),单日一次扫描;
// 结果按 (factorID, day) 缓存,同一回测日重复调用不重复扫描。
func DailyValues(l *lake.Lake, def *Definition, day int64) (map[string]float64, error) {
	key := def.ID + "|" + formatDay(day)
	dailyMu.Lock()
	if cached, ok := dailyCache[key]; ok {
		dailyMu.Unlock()
		return cached, nil
	}
	dailyMu.Unlock()

	window := int64(def.Window())
	// 因子窗口 + 少量余量(按自然日粗放取,交易日足够)
	panel, err := LoadPanel(l, day-window*2-5, day)
	if err != nil {
		return nil, err
	}
	values := panel.Values(def, day)
	if values == nil {
		values = map[string]float64{}
	}

	dailyMu.Lock()
	if _, exists := dailyCache[key]; !exists {
		dailyCache[key] = values
		dailyOrder = append(dailyOrder, key)
		if len(dailyOrder) > dailyCacheMax {
			oldest := dailyOrder[0]
			dailyOrder = dailyOrder[1:]
			delete(dailyCache, oldest)
		}
	}
	dailyMu.Unlock()
	return values, nil
}

func formatDay(day int64) string {
	return time.Unix(day*86400, 0).UTC().Format("20060102")
}

// CacheSize 返回单日值缓存条目数(观测用)。
func CacheSize() int {
	dailyMu.Lock()
	defer dailyMu.Unlock()
	return len(dailyCache)
}
