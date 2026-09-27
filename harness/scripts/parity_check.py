# -*- coding: utf-8 -*-
"""对拍引擎:同一策略在 quantbt 与 Go 引擎上运行,逐项对比。

用法:
    python harness/scripts/parity_check.py --strategy examples/strategies/parity/p01_market_basic.py \
        [--start 2020-01-02] [--end 2020-01-10] [--capital 1000000] [--warmup-days 30] \
        [--frequency 1d] [--quant-data E:\AI-work\quant-data] [--lake D:\quant-lake]

对比项:净值序列、委托字段、成交字段、期末资产与持仓;输出首个差异。
分钟对拍:
    python harness/scripts/parity_check.py --frequency 1m --warmup-bars 5 \
        --strategy E:\AI-work\quant-data\strategies\probes\ptrade_alignment_limit_order.py \
        --start 2020-01-02 --end 2020-01-03
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

# 委托编号是随机串(quantbt 用 uuid4().hex),日志对比前归一化
_ORDER_ID_RE = re.compile(r"\b[0-9a-f]{32}\b")


def normalize_log(message: str) -> str:
    return _ORDER_ID_RE.sub("<id>", message or "")


def run_quantbt(strategy: str, start: str, end: str, capital: float, warmup_days: int,
                warmup_bars: int, frequency: str, quant_data: Path) -> dict:
    """在 quantbt(参照实现)上运行回测,返回标准化的结果。"""
    code = f'''
import json, sys
sys.path.insert(0, r"{quant_data}")
from pathlib import Path
from quantbt import Backtest

result = Backtest(
    strategy_path=Path(r"{strategy}"),
    data_root=Path(r"{quant_data}") / "quant-store",
    start_date="{start}",
    end_date="{end}",
    frequency="{frequency}",
    capital_base={capital},
    warmup_days={warmup_days},
    warmup_bars={warmup_bars},
).run()

date_key = "datetime" if "datetime" in result.portfolio.columns else "date"
portfolio = [
    {{"date": str(r[date_key]), "value": float(r["portfolio_value"]), "cash": float(r["cash"])}}
    for _, r in result.portfolio.iterrows()
]
orders = [
    {{"dt": o.dt.strftime("%Y-%m-%d %H:%M:%S"), "symbol": o.symbol,
      "amount": float(o.amount), "filled": float(o.filled), "status": str(o.status),
      "limit": float(o.limit or 0.0), "filled_price": float(getattr(o, "filled_price", 0.0) or 0.0)}}
    for o in result.orders
]
trades = []
for order_id, rows in (result.trades or {{}}).items():
    for tr in rows:
        trades.append({{
            "order_id": order_id, "security": tr[2], "side": tr[3],
            "amount": float(tr[4]), "price": float(tr[5]), "value": float(tr[6]),
        }})
logs = [{{"level": l.get("level"), "message": l.get("message")}} for l in (result.logs or [])]
print("###PARITY###" + json.dumps({{"portfolio": portfolio, "orders": orders,
                                     "trades": trades, "logs": logs}}, ensure_ascii=False))
'''
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False, encoding="utf-8") as fh:
        fh.write(code)
        script = fh.name
    try:
        env = dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUTF8="1")
        proc = subprocess.run([sys.executable, script], capture_output=True, text=True,
                              encoding="utf-8", errors="replace", timeout=600, env=env)
    finally:
        os.unlink(script)
    if proc.returncode != 0:
        raise RuntimeError(f"quantbt 运行失败:\n{proc.stdout[-2000:]}\n{proc.stderr[-2000:]}")
    for line in proc.stdout.splitlines():
        if line.startswith("###PARITY###"):
            return json.loads(line[len("###PARITY###"):])
    raise RuntimeError(f"quantbt 未输出结果:\n{proc.stdout[-2000:]}")


def run_go_engine(strategy: str, start: str, end: str, capital: float, warmup_days: int,
                  frequency: str, lake: str, binary: str) -> dict:
    """在 Go 引擎上运行同一策略。"""
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8") as fh:
        out_path = fh.name
    try:
        proc = subprocess.run(
            [binary, "backtest", "--strategy", strategy, "--start", start.replace("-", ""),
             "--end", end.replace("-", ""), "--capital", str(capital), "--warmup", str(warmup_days),
             "--frequency", frequency, "--lake", lake, "--out", out_path],
            capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=1800,
        )
        if proc.returncode != 0:
            raise RuntimeError(f"Go 引擎运行失败:\n{proc.stdout[-2000:]}\n{proc.stderr[-2000:]}")
        data = json.loads(Path(out_path).read_text(encoding="utf-8"))
    finally:
        if os.path.exists(out_path):
            os.unlink(out_path)
    portfolio = [{"date": r["date"], "value": r["portfolio_value"], "cash": r["cash"]}
                 for r in data["portfolio"]]
    orders = []
    for o in (data.get("orders") or []):
        created = str(o["created_at"])
        dt = created if " " in created else f"{created} 15:00:00"
        orders.append({"dt": dt, "symbol": o["security"],
                       "amount": float(o["amount"]), "filled": float(o["filled"]),
                       "status": str(o["status"]),
                       "limit": float(o["limit_price"]), "filled_price": float(o["filled_price"] or 0.0)})
    trades = [{"order_id": t["order_id"], "security": t["security"],
               "side": "买" if t["side"] == "buy" else "卖",
               "amount": float(t["amount"]), "price": float(t["price"]), "value": float(t["value"])}
              for t in (data.get("trades") or [])]
    logs = [{"level": l["level"], "message": l["message"]} for l in data["logs"]]
    return {"portfolio": portfolio, "orders": orders, "trades": trades, "logs": logs}


def compare(reference: dict, subject: dict, tolerance: float = 1e-6) -> list[str]:
    diffs: list[str] = []

    def near(a: float, b: float) -> bool:
        return abs(a - b) <= tolerance * max(1.0, abs(a), abs(b))

    ref_pf, sub_pf = reference["portfolio"], subject["portfolio"]
    if len(ref_pf) != len(sub_pf):
        diffs.append(f"净值序列长度不同: quantbt={len(ref_pf)} go={len(sub_pf)}")
    for i, (a, b) in enumerate(zip(ref_pf, sub_pf)):
        if a["date"] != b["date"]:
            diffs.append(f"净值[{i}]日期不同: {a['date']} vs {b['date']}")
            break
        if not near(a["value"], b["value"]):
            diffs.append(f"净值[{i}] {a['date']} 不同: quantbt={a['value']:.6f} go={b['value']:.6f}")
            break
        if not near(a["cash"], b["cash"]):
            diffs.append(f"现金[{i}] {a['date']} 不同: quantbt={a['cash']:.6f} go={b['cash']:.6f}")
            break

    ref_orders, sub_orders = reference["orders"], subject["orders"]
    if len(ref_orders) != len(sub_orders):
        diffs.append(f"委托数量不同: quantbt={len(ref_orders)} go={len(sub_orders)}")
    for i, (a, b) in enumerate(zip(ref_orders, sub_orders)):
        for key in ("dt", "symbol", "status"):
            if str(a[key]) != str(b[key]):
                diffs.append(f"委托[{i}].{key} 不同: quantbt={a[key]} go={b[key]}")
        for key in ("amount", "filled", "limit"):
            if not near(float(a[key]), float(b[key])):
                diffs.append(f"委托[{i}].{key} 不同: quantbt={a[key]} go={b[key]}")

    ref_trades, sub_trades = reference["trades"], subject["trades"]
    if len(ref_trades) != len(sub_trades):
        diffs.append(f"成交数量不同: quantbt={len(ref_trades)} go={len(sub_trades)}")
    for i, (a, b) in enumerate(zip(ref_trades, sub_trades)):
        for key in ("security", "side"):
            if str(a[key]) != str(b[key]):
                diffs.append(f"成交[{i}].{key} 不同: quantbt={a[key]} go={b[key]}")
        for key in ("amount", "price", "value"):
            if not near(float(a[key]), float(b[key])):
                diffs.append(f"成交[{i}].{key} 不同: quantbt={a[key]} go={b[key]}")
    return diffs


def main() -> int:
    parser = argparse.ArgumentParser(description="quantbt 与 Go 引擎对拍")
    parser.add_argument("--strategy", default="", help="策略文件路径(--all 时可省略)")
    parser.add_argument("--start", default="2020-01-02")
    parser.add_argument("--end", default="2020-01-10")
    parser.add_argument("--capital", type=float, default=1_000_000)
    parser.add_argument("--warmup-days", type=int, default=30)
    parser.add_argument("--go-warmup", type=int, default=0,
                        help="Go 引擎预热天数(分钟模式默认 30;0 表示与 --warmup-days 相同)")
    parser.add_argument("--warmup-bars", type=int, default=5, help="分钟对拍的预热 Bar 数(quantbt)")
    parser.add_argument("--frequency", default="1d", choices=["1d", "1m"])
    parser.add_argument("--quant-data", default=r"E:\AI-work\quant-data")
    parser.add_argument("--lake", default=r"D:\quant-lake")
    parser.add_argument("--binary", default=None, help="quantd 可执行文件(默认 bin/quantd.exe)")
    parser.add_argument("--show-logs", action="store_true", help="打印两侧日志")
    parser.add_argument("--check-logs", action="store_true", help="对比策略日志(message 序列必须一致)")
    parser.add_argument("--all", action="store_true", help="批量运行 parity/ 目录下的全部探针")
    parser.add_argument("--cases", default="", help="用例集 JSON(quant-data 对齐测试的探针+参数)")
    parser.add_argument("--quant-probes", action="store_true",
                        help="批量运行 quant-data 的 ptrade 对齐探针(需 --frequency)")
    args = parser.parse_args()

    if args.all:
        probe_dir = Path(__file__).resolve().parents[2] / "examples" / "strategies" / "parity"
        probes = sorted(probe_dir.glob("p*.py"))
        failed = 0
        for probe in probes:
            code = run_one(str(probe), args)
            if code != 0:
                failed += 1
        print()
        print(f"批量对拍: {len(probes)} 个探针, {len(probes) - failed} 通过, {failed} 失败")
        return 1 if failed else 0

    if args.quant_probes:
        probe_dir = Path(args.quant_data) / "strategies" / "probes"
        probes = sorted(probe_dir.glob("ptrade_alignment_*.py"))
        failed = 0
        for probe in probes:
            code = run_one(str(probe), args)
            if code != 0:
                failed += 1
        print()
        print(f"批量对拍(quant 探针): {len(probes)} 个探针, {len(probes) - failed} 通过, {failed} 失败")
        return 1 if failed else 0

    if args.cases:
        cases = json.loads(Path(args.cases).read_text(encoding="utf-8"))
        failed = 0
        for case in cases:
            strategy = str(Path(args.quant_data) / "strategies" / "probes" / case["probe"])
            code = run_one(strategy, args, case)
            if code != 0:
                failed += 1
        print()
        print(f"批量对拍(用例集): {len(cases)} 个用例, {len(cases) - failed} 通过, {failed} 失败")
        return 1 if failed else 0

    return run_one(args.strategy, args)


def run_one(strategy: str, args, case: dict | None = None) -> int:
    binary = args.binary or str(Path(__file__).resolve().parents[2] / "bin" / "quantd.exe")
    frequency = (case or {}).get("frequency", args.frequency)
    start = (case or {}).get("start", args.start)
    end = (case or {}).get("end", args.end)
    capital = (case or {}).get("capital", args.capital)
    warmup_days = int((case or {}).get("warmup_days", args.warmup_days))
    warmup_bars = int((case or {}).get("warmup_bars", args.warmup_bars))
    go_warmup = args.go_warmup or (30 if frequency == "1m" else warmup_days)
    label = (case or {}).get("test") or Path(strategy).name
    print(f"== 对拍: {label} ({frequency}) ==")
    print(f"   策略: {strategy}")
    print(f"   quantbt: {args.quant_data}/quant-store")
    print(f"   go    : {args.lake}")

    reference = run_quantbt(strategy, start, end, capital,
                            warmup_days, warmup_bars, frequency,
                            Path(args.quant_data))
    subject = run_go_engine(strategy, start, end, capital,
                            go_warmup, frequency, args.lake, binary)

    print(f"   quantbt: 净值 {len(reference['portfolio'])} 条,委托 {len(reference['orders'])} 笔,"
          f"成交 {len(reference['trades'])} 笔")
    print(f"   go     : 净值 {len(subject['portfolio'])} 条,委托 {len(subject['orders'])} 笔,"
          f"成交 {len(subject['trades'])} 笔")

    if args.show_logs:
        print("\n--- quantbt 日志 ---")
        for log in reference["logs"]:
            print(f"  [{log['level']}] {log['message']}")
        print("--- go 日志 ---")
        for log in subject["logs"]:
            print(f"  [{log['level']}] {log['message']}")

    diffs = compare(reference, subject)
    if args.check_logs:
        ref_logs = [(l["level"], normalize_log(l["message"])) for l in reference["logs"]]
        sub_logs = [(l["level"], normalize_log(l["message"])) for l in subject["logs"]]
        if ref_logs != sub_logs:
            diffs.append(f"日志序列不同: quantbt={len(ref_logs)} 条 go={len(sub_logs)} 条")
            for i, (a, b) in enumerate(zip(ref_logs, sub_logs)):
                if a != b:
                    diffs.append(f"  日志[{i}]: quantbt={a} go={b}")
                    break
    print()
    if diffs:
        print(f"FAIL: {len(diffs)} 处差异")
        for line in diffs[:15]:
            print("  -", line)
        return 1
    print("PASS: 净值/委托/成交逐项一致")
    return 0


if __name__ == "__main__":
    sys.exit(main())
