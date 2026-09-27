# -*- coding: utf-8 -*-
"""回测性能基准:同一股票池下测量日线/分钟回测耗时,用于优化前后对比。

用法:
    python harness/scripts/bench_backtest.py --stocks 300 --start 20200102 --end 20200131
    python harness/scripts/bench_backtest.py --stocks 300 --access all --frequency 1m

参数:
    --stocks N      股票池大小(取 bars_daily 中年份区间内前 N 只,默认 300)
    --access none   策略不访问行情(默认)
             some   每分钟访问前 3 只
             all    每分钟遍历全部(最坏情况)
    --frequency     1m / 1d
    --warmup        预热交易日数(分钟模式窗口大小)

输出:各阶段耗时与 bar 总数,便于回归对比(见 harness/experience/backtest-performance.md)。
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def load_codes(lake: str, year: int, count: int) -> list[str]:
    """从数据湖读取指定年份的前 N 只股票代码(用 lakediag 的 go 依赖过重,这里直接扫描文件头)。"""
    import duckdb  # 本地基准脚本允许使用 duckdb

    files = sorted(Path(lake).glob(f"canonical/bars_daily/year={year}/*.parquet"))
    if not files:
        raise SystemExit(f"未找到 {lake}/canonical/bars_daily/year={year} 数据")
    con = duckdb.connect()
    rows = con.execute(
        "select distinct ts_code from read_parquet(?) order by ts_code limit ?",
        [str(files[0]), count],
    ).fetchall()
    return [r[0] for r in rows]


def strategy_source(codes: list[str], access: str) -> str:
    universe = ", ".join('"%s"' % c for c in codes)
    body = {
        "none": "    pass",
        "some": '    for code in ("%s", "%s", "%s"):\n        g.total += data[code].close'
        % (codes[0], codes[1], codes[2]),
        "all": "    for code in data:\n        g.total += data[code].close",
    }[access]
    return (
        "def initialize(context):\n"
        "    set_universe([%s])\n"
        "    g.total = 0.0\n"
        "\n"
        "def handle_data(context, data):\n"
        "%s\n"
        "\n"
        "def after_trading_end(context, data):\n"
        '    log.info("BENCH|total=" + str(round(g.total, 2)))\n'
    ) % (universe, body)


def main() -> int:
    parser = argparse.ArgumentParser(description="回测性能基准")
    parser.add_argument("--stocks", type=int, default=300)
    parser.add_argument("--start", default="20200102")
    parser.add_argument("--end", default="20200131")
    parser.add_argument("--frequency", default="1m", choices=["1m", "1d"])
    parser.add_argument("--warmup", type=int, default=30)
    parser.add_argument("--access", default="none", choices=["none", "some", "all"])
    parser.add_argument("--lake", default=r"D:\quant-lake")
    parser.add_argument("--binary", default=str(ROOT / "bin" / "quantd.exe"))
    args = parser.parse_args()

    codes = load_codes(args.lake, int(args.start[:4]), args.stocks)
    source = strategy_source(codes, args.access)
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, encoding="utf-8") as fh:
        fh.write(source)
        strategy = fh.name
    out = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
    out.close()

    cmd = [
        args.binary, "backtest", "--strategy", strategy,
        "--start", args.start, "--end", args.end,
        "--warmup", str(args.warmup), "--frequency", args.frequency,
        "--lake", args.lake, "--out", out.name,
    ]
    t0 = time.time()
    proc = subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace")
    elapsed = time.time() - t0
    os.unlink(strategy)
    try:
        data = json.loads(Path(out.name).read_text(encoding="utf-8"))
        bars = len(data.get("portfolio") or [])
    except Exception:  # noqa: BLE001
        bars = 0
    finally:
        os.unlink(out.name)

    print(f"股票池={len(codes)} 频率={args.frequency} 区间={args.start}~{args.end} "
          f"预热={args.warmup} 访问={args.access}")
    print(f"耗时={elapsed:.1f}s 净值行={bars} rc={proc.returncode}")
    if proc.returncode != 0:
        print((proc.stderr or proc.stdout or "")[-500:])
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
