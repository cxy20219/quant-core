# 回测性能经验(分钟 / 大股票池)

## 现象

分钟回测在大股票池下明显变慢:300 只 × 16 个交易日(2020-01)全链路 **70s**;
1 只同样区间只要 3.7s。日线同池同区间 8.6s,说明瓶颈在"每分钟 × 池大小"的路径上。

## 根因(按影响排序,均已修复)

1. **扫描层 `IN` 谓词逐行线性比较**(占引擎 58%):`Predicate.matchAt` 对每个
   `ts_code IN (...)` 逐行遍历取值列表做字符串比较,300 只股票池时每行最多 300 次比较。
   → 改为谓词级惰性哈希索引(`inSet/inInt/inFlt`),行匹配降为一次 map 查找;
   行组统计剪枝的 `IN` 物理值也缓存排序结果(原先每个行组重复转换 + 插入排序)。
   效果:引擎 50.9s → 19.3s。
2. **扫描器每行两次切片分配**(占 12.8% + 约 10% GC):`convertRow` 每行 `make`
   逻辑值行,`Row()` 再 `make` 投影行。
   → 复用 `rowBuf/outBuf`;契约变为"返回切片在下一次 `Next/Row` 前有效"
   (调用方需自行拷贝保留,测试已同步)。
   效果:引擎 19.3s → 16.7s。
3. **每分钟向 Python 传全量 bar(字段名重复)**:300 只 × 3840 分钟 ≈ 170MB JSON,
   且策略侧每分钟构造 300 个 bar 对象。
   → 列式载荷(`codes` + `series` 字段数组)+ 惰性 `BarDict`(与 quantbt 的
   `BarDict` 表面一致:`[]`/`in`/迭代,按需构建单个 bar)。
   效果:全链路 38.4s → 20.3s;遍历全部 bar 的最坏情况 23.0s。

## 效果对照(300 只 × 2020-01-02~01-31,1m,预热 30 天)

| 场景 | 优化前 | 优化后 |
|---|---|---|
| 引擎(Go,noop runner) | 50.9s | 16.7s |
| 全链路(策略不访问行情) | 69.9s | 20.2s |
| 全链路(每分钟访问 3 只) | — | 20.2s |
| 全链路(每分钟遍历全部 300 只) | — | 23.0s |
| 日线同池同区间(对照) | 8.6s | 4.2s |

## 做法

- 先分层测量,再优化:

  ```bash
  # 1) 全链路(日线/分钟、不同行情访问模式)
  python harness/scripts/bench_backtest.py --stocks 300 --access none
  python harness/scripts/bench_backtest.py --stocks 300 --access all --frequency 1m

  # 2) 仅引擎(排除 Python 子进程)
  BENCH_MINUTE=1 go test ./internal/btengine -run TestMinuteEngineBench -v -timeout 900s

  # 3) 热点(CPU profile)
  BENCH_PROFILE=/tmp/bench.prof BENCH_MINUTE=1 go test ./internal/btengine -run TestMinuteEngineBench -timeout 900s
  go tool pprof -top /tmp/bench.prof
  ```

验证:基准脚本输出 `耗时=..s 净值行=.. rc=0`;
分钟 300 只 × 16 交易日应 ≈20s(本地),明显变慢说明退化。
- 判读:若"仅引擎"远快于全链路 → 瓶颈在 Python 子进程/协议;反之在扫描或引擎循环。
  行组读取与 zstd 解码属于固有成本,profile 里占比高但不宜再优化。
- 大池长区间仍受窗口内存限制:窗口 = 预热天数 × 池大小 × 240 Bar,
  按需调小 `--warmup`(分钟历史通常只需最近 1~2 天)。

## 边界

- 遍历全部 bar 的最坏情况仍是每分钟 池大小 次对象构造;若策略需要全市场横截面,
  优先用 `get_history`/`get_fundamentals` 批量接口(引擎侧一次查询),而不是遍历 `data`。
- 窗口内存未做上限保护:2000 只 × 30 天 ≈ 1400 万 Bar(约 1GB),NAS 上需注意。
- NAS 容器 CPU 弱于开发机,同一基准耗时约为本地的 1.5~2 倍。

## 关联

- 基准脚本:`harness/scripts/bench_backtest.py`
- 扫描引擎:`harness/experience/scan-engine-pruning.md`(剪枝与行组布局)
- 对拍回归:`harness/workflow/bt-parity.md`(优化后必须全量回归)
