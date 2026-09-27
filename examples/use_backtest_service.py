"""回测服务使用示例:提交 Python 策略 + 参数,轮询结果。

用法:
    python examples/use_backtest_service.py                          # 默认 NAS
    python examples/use_backtest_service.py http://127.0.0.1:18020   # 本地
"""

from __future__ import annotations

import json
import sys
import time
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://<nas-host>:8000"

# PTrade 兼容策略:双均线,参数通过 params 注入
STRATEGY = '''
def initialize(context):
    g.code = params.get("code", "600000.SH")
    g.fast = int(params.get("fast", 5))
    g.slow = int(params.get("slow", 20))
    set_universe([g.code])
    set_commission(commission_ratio=0.0003, min_commission=5.0)
    set_slippage(0.002)
    log.info("策略启动 %s fast=%s slow=%s", g.code, g.fast, g.slow)


def handle_data(context, data):
    if g.code not in data:
        return
    hist = get_history(g.slow + 1, "1d", "close", g.code, fq="post", include=True)
    if len(hist) < g.slow + 1:
        return
    closes = hist["close"]
    fast = closes[-g.fast:].mean()
    slow = closes[-g.slow:].mean()
    pos = get_position(g.code)
    if fast > slow and pos.amount == 0:
        order_target_value(g.code, context.portfolio.portfolio_value * 0.95)
    elif fast < slow and pos.amount > 0:
        order_target(g.code, 0)
'''


def submit(payload: dict) -> dict:
    req = urllib.request.Request(f"{BASE}/api/backtests", data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.loads(resp.read().decode())


def get(path: str) -> dict:
    with urllib.request.urlopen(BASE + path, timeout=60) as resp:
        return json.loads(resp.read().decode())


def main() -> int:
    print(f"服务地址: {BASE}")
    job = submit({
        "strategy_code": STRATEGY,
        "strategy_name": "示例-双均线",
        "params": {"code": "600000.SH", "fast": 5, "slow": 20},
        "start_date": "20240101",
        "end_date": "20241231",
        "capital_base": 1_000_000,
        "benchmark": "000300.SH",
    })
    print(f"已提交: {job['id']}")

    while True:
        record = get(f"/api/backtests/{job['id']}")
        if record["status"] in ("done", "failed"):
            break
        time.sleep(1)

    if record["status"] == "failed":
        print("失败:", record.get("error"))
        return 1

    result = record["result"]
    summary = result["summary"]
    analytics = result["analytics"]
    print(f"\n区间: {result['start_date']} ~ {result['end_date']}")
    print(f"收益: {summary['total_return']:.2%}(期末 {summary['final_value']:,.0f})")
    print(f"年化: {analytics['annualized_return']:.2%}  "
          f"夏普: {analytics['sharpe_ratio']:.2f}  "
          f"最大回撤: {analytics['max_drawdown']:.2%}")
    print(f"委托 {summary['order_count']} 笔 / 成交 {summary['trade_count']} 笔,"
          f"用时 {summary['elapsed_seconds']:.1f}s")
    print("\n最近日志:")
    for log in result["logs"][-5:]:
        print(f"  [{log['level']}] {log['message']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
