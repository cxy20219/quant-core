"""quantd API 基准测试:用真实 tushare SDK 打 tushare 兼容接口,统计延迟分位。

用法:
    python tools/bench_api.py --url http://127.0.0.1:8000 --daily-reps 30 --mins-reps 10
"""

from __future__ import annotations

import argparse
import statistics
import time
import warnings

warnings.filterwarnings("ignore")

import tushare as ts


def bench(fn, reps: int) -> list[float]:
    latencies = []
    for _ in range(reps):
        start = time.perf_counter()
        fn()
        latencies.append((time.perf_counter() - start) * 1000)
    return latencies


def report(name: str, latencies: list[float], rows: int) -> None:
    latencies = sorted(latencies)
    p50 = latencies[len(latencies) // 2]
    p95 = latencies[min(len(latencies) - 1, int(len(latencies) * 0.95))]
    print(f"{name:<38} n={len(latencies):<3} rows={rows:<7} "
          f"p50={p50:7.1f}ms p95={p95:7.1f}ms max={latencies[-1]:7.1f}ms")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:8000")
    parser.add_argument("--daily-reps", type=int, default=30)
    parser.add_argument("--mins-reps", type=int, default=10)
    parser.add_argument("--token", default="bench")
    args = parser.parse_args()

    pro = ts.pro_api(args.token)
    pro._DataApi__http_url = args.url

    print(f"target: {args.url}")

    # 1) 单代码一年日线
    lat = bench(lambda: pro.daily(ts_code="600000.SH", start_date="20240101", end_date="20241231"),
                args.daily_reps)
    report("daily 1 code / 1 year", lat, len(pro.daily(ts_code="600000.SH", start_date="20240101", end_date="20241231")))

    # 2) 单代码十年日线
    lat = bench(lambda: pro.daily(ts_code="600000.SH", start_date="20150101", end_date="20241231"),
                max(5, args.daily_reps // 3))
    report("daily 1 code / 10 years", lat, len(pro.daily(ts_code="600000.SH", start_date="20150101", end_date="20241231")))

    # 3) 全市场单日
    lat = bench(lambda: pro.daily(trade_date="20240102"), max(5, args.daily_reps // 3))
    report("daily all market / 1 day", lat, len(pro.daily(trade_date="20240102")))

    # 4) 单代码单日分钟
    lat = bench(lambda: pro.stk_mins(ts_code="600000.SH", freq="1min",
                                     start_date="20240102 09:00:00", end_date="20240102 15:30:00"),
                args.mins_reps)
    report("stk_mins 1 code / 1 day", lat,
           len(pro.stk_mins(ts_code="600000.SH", freq="1min",
                            start_date="20240102 09:00:00", end_date="20240102 15:30:00")))

    # 5) 单代码一个月分钟
    lat = bench(lambda: pro.stk_mins(ts_code="600000.SH", freq="1min",
                                     start_date="20240101 09:00:00", end_date="20240131 15:30:00"),
                max(3, args.mins_reps // 3))
    report("stk_mins 1 code / 1 month", lat,
           len(pro.stk_mins(ts_code="600000.SH", freq="1min",
                            start_date="20240101 09:00:00", end_date="20240131 15:30:00")))

    # 6) 基本信息
    lat = bench(lambda: pro.stock_basic(exchange="", list_status="L"), args.daily_reps)
    report("stock_basic listed", lat, 0)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
