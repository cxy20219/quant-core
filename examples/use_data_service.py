"""quant-core 数据服务使用示例(全部接口)。

把 tushare SDK 的 HTTP 地址指向 quantd 即可,接口名、参数、字段与 tushare 一致。

运行:
    python examples/use_data_service.py                        # 默认 NAS 地址
    python examples/use_data_service.py http://127.0.0.1:8000  # 本地湖
"""

from __future__ import annotations

import sys
import warnings

warnings.filterwarnings("ignore")

import tushare as ts


def make_pro(url: str):
    pro = ts.pro_api("local")
    # tushare SDK 通过这个私有属性指定 HTTP 入口;quantd 实现了同一协议。
    pro._DataApi__http_url = url
    return pro


def main() -> int:
    url = sys.argv[1] if len(sys.argv) > 1 else "http://<nas-host>:8000"
    pro = make_pro(url)
    print(f"服务地址: {url}\n")

    # ── 行情类 ────────────────────────────────────────────────
    print("== daily 日线行情(2000 年起) ==")
    df = pro.daily(ts_code="600000.SH", start_date="20240102", end_date="20240110")
    print(df.to_string(index=False))
    print("  单位: vol=手, amount=千元, pct_chg=百分数\n")

    print("== daily_basic 每日指标(估值/市值) ==")
    df = pro.daily_basic(ts_code="600000.SH", trade_date="20240102",
                         fields="ts_code,trade_date,close,turnover_rate,pe,pb,total_mv")
    print(df.to_string(index=False))
    print("  单位: total_mv=万元, total_share=万股, turnover_rate=%\n")

    print("== adj_factor 复权因子(默认后复权 hfq) ==")
    df = pro.adj_factor(ts_code="600000.SH", start_date="20240101", end_date="20240110")
    print(df.to_string(index=False))
    print("  前复权: 传 factor_type='qfq'\n")

    print("== stk_mins 分钟线(仅 1min) ==")
    df = pro.stk_mins(ts_code="600000.SH", freq="1min",
                      start_date="2026-09-01 09:30:00", end_date="2026-09-01 10:00:00")
    print(df.head(5).to_string(index=False))
    print("  单位: vol=股, amount=元(注意与日线不同)\n")

    # ── 基础信息类 ────────────────────────────────────────────
    print("== stock_basic 股票列表 ==")
    df = pro.stock_basic(exchange="", list_status="L", fields="ts_code,name,industry,list_date")
    print(f"  上市 {len(df)} 只;示例:")
    print(df.head(3).to_string(index=False))
    print("  list_status: L=上市 D=退市 P=暂停上市\n")

    print("== trade_cal 交易日历 ==")
    df = pro.trade_cal(exchange="SSE", start_date="20260901", end_date="20260927")
    print(df.tail(5).to_string(index=False))
    print("  is_open: 1=交易 0=休市;exchange 支持 SSE/SZSE/CFFEX\n")

    print("== index_basic 指数基本信息 ==")
    df = pro.index_basic(market="SSE", fields="ts_code,name,index_type,list_date")
    print(f"  SSE 指数 {len(df)} 只;示例:")
    print(df.head(3).to_string(index=False))
    print("  market: SSE/SZSE/CSI/CNI/SW\n")

    print("== index_daily 指数日线(10 个宽基指数) ==")
    df = pro.index_daily(ts_code="000300.SH", start_date="20240101", end_date="20240110")
    print(df.to_string(index=False))

    # ── 交易状态类 ────────────────────────────────────────────
    print("\n== suspend_d 停复牌 ==")
    df = pro.suspend_d(start_date="20240101", end_date="20240131")
    print(f"  2024-01 停复牌 {len(df)} 条;示例:")
    print(df.head(3).to_string(index=False))
    print("  suspend_type: S=停牌 R=复牌\n")

    print("== namechange 名称变更 ==")
    df = pro.namechange(start_date="20240101", end_date="20241231")
    print(f"  2024 年 {len(df)} 条;示例:")
    print(df.head(3).to_string(index=False))

    print("== stk_limit 涨跌停价(部分覆盖,建设中) ==")
    df = pro.stk_limit(ts_code="600000.SH", start_date="20250101", end_date="20250110")
    print(df.to_string(index=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
