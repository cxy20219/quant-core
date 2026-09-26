"""NAS 部署验收:健康检查 + tushare 兼容接口 + 延迟基准。

用法:
    python tools/accept_deploy.py --url http://<nas-host>:8000 [--full]
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.request
import warnings

warnings.filterwarnings("ignore")

import tushare as ts


def healthz(base: str) -> dict:
    with urllib.request.urlopen(base + "/healthz", timeout=15) as resp:
        return json.loads(resp.read().decode())


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://<nas-host>:8000")
    parser.add_argument("--full", action="store_true", help="跑完整基准(约 1 分钟)")
    args = parser.parse_args()

    base = args.url.rstrip("/")
    print(f"== 健康检查 {base}/healthz ==")
    info = healthz(base)
    print(json.dumps(info, ensure_ascii=False, indent=2))
    if info.get("status") != "ok":
        print("FAIL: health status")
        return 1

    pro = ts.pro_api("accept")
    pro._DataApi__http_url = base
    failures = []

    def check(name, fn, expect_rows=None, min_rows=0):
        started = time.perf_counter()
        try:
            df = fn()
        except Exception as exc:  # noqa: BLE001
            failures.append(f"{name}: {exc}")
            print(f"FAIL {name}: {exc}")
            return None
        elapsed = (time.perf_counter() - started) * 1000
        ok = True
        if expect_rows is not None and len(df) != expect_rows:
            ok = False
        if len(df) < min_rows:
            ok = False
        status = "OK  " if ok else "FAIL"
        if not ok:
            failures.append(f"{name}: rows={len(df)} expect={expect_rows} min={min_rows}")
        print(f"{status} {name:<42} rows={len(df):<6} {elapsed:7.0f}ms")
        return df

    print("\n== 接口验收 ==")
    df = check("daily 600000.SH 2024 全年", lambda: pro.daily(ts_code="600000.SH", start_date="20240101", end_date="20241231"), expect_rows=242)
    if df is not None and len(df):
        row = df.iloc[0]
        if row["trade_date"] != "20240102" or abs(row["close"] - 7.36) > 0.01:
            print(f"    首行校验: {row['trade_date']} close={row['close']}")

    check("daily 全市场单日", lambda: pro.daily(trade_date="20240102"), min_rows=5000)
    check("daily_basic 单日单票", lambda: pro.daily_basic(ts_code="600000.SH", trade_date="20240102"), expect_rows=1)
    check("adj_factor 2024", lambda: pro.adj_factor(ts_code="600000.SH", start_date="20240101", end_date="20241231"), expect_rows=242)
    check("stk_mins 单日", lambda: pro.stk_mins(ts_code="600000.SH", freq="1min", start_date="2024-01-02 09:00:00", end_date="2024-01-02 15:30:00"), expect_rows=241)
    check("stk_mins 单月", lambda: pro.stk_mins(ts_code="600000.SH", freq="1min", start_date="2024-01-01 09:00:00", end_date="2024-01-31 15:30:00"), min_rows=4000)
    check("daily 十年跨度", lambda: pro.daily(ts_code="600000.SH", start_date="20150101", end_date="20241231"), min_rows=2000)

    # 单位校验:分钟线 vol 单位为股(手×100)
    df = pro.stk_mins(ts_code="600000.SH", freq="1min", start_date="2024-01-02 09:30:00", end_date="2024-01-02 09:31:00")
    if len(df) and df.iloc[0]["vol"] > 100000:
        print("NOTE vol 单位可能未按股输出:", df.iloc[0]["vol"])
        failures.append("stk_mins vol unit")

    if args.full:
        print("\n== 延迟基准 ==")
        for name, fn, reps in [
            ("daily 单代码一年", lambda: pro.daily(ts_code="600000.SH", start_date="20240101", end_date="20241231"), 30),
            ("stk_mins 单代码单日", lambda: pro.stk_mins(ts_code="600000.SH", freq="1min", start_date="2024-01-02 09:00:00", end_date="2024-01-02 15:30:00"), 15),
        ]:
            lat = []
            for _ in range(reps):
                t0 = time.perf_counter()
                fn()
                lat.append((time.perf_counter() - t0) * 1000)
            lat.sort()
            print(f"  {name:<24} p50={lat[len(lat)//2]:6.1f}ms p95={lat[min(len(lat)-1, int(len(lat)*0.95))]:6.1f}ms")

    print()
    if failures:
        print(f"FAILED: {len(failures)} 项 -> {failures}")
        return 1
    print("ALL PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
