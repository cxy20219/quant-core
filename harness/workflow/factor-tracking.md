# 因子管理 / 因子跟踪流程

## 适用场景

注册因子口径、校验覆盖率、评估因子的 IC / 分层收益(为策略选因子与择时提供依据)。
因子计算基于湖内**截面物化表 `daily_cross`**(按 `(trade_date, ts_code)` 排序),
单日全市场一次扫描;缺失年份自动回退 `bars_daily`。

## 前置条件

- 数据湖含 `daily_cross`(由 `quantd build-cross` 物化,见 `harness/workflow/deploy-nas.md`);
- 服务已启动(`quantd serve`),因子接口挂在 `/api/factors`(与面板同进程)。

## 关键步骤

1. **注册因子**(注册表落 `<lake>/meta/factors.json`):

   ```bash
   curl -s -X POST http://HOST:8000/api/factors -H 'Content-Type: application/json' \
     -d '{"id":"momentum_20","name":"20日动量","kind":"momentum","params":{"window":20},
          "description":"close/close[-20]-1","direction":"positive"}'
   ```

   内置类型(`GET /api/factors/catalog`):
   `momentum` / `reversal` / `volatility` / `turnover` / `size` / `field`(直接取 `pb`、`pe_ttm`、`circ_mv` 等字段)。

2. **查看列表与覆盖率**:

   ```bash
   curl -s http://HOST:8000/api/factors | head -c 400
   curl -s "http://HOST:8000/api/factors/size_log/coverage?start=20241101&end=20241231"
   ```

3. **跟踪(IC / RankIC / 分层收益)**:

   ```bash
   curl -s "http://HOST:8000/api/factors/momentum_20/tracking?start=20230101&end=20241231&horizon=5&layers=5"
   ```

   返回:`dates/ic/rank_ic/coverage` 逐日序列、`layer_ret`(每层单期平均前向收益)、
   `layer_cum`(每层累计)、`stats`(ic_mean/ic_std/ir/rank_ic_mean/ic_positive_ratio/long_short_cum)。

4. **面板查看**(推荐):浏览器打开 `/panel` → “因子”页签:
   注册表列表(注册/删除)、跟踪视图(IC/RankIC 曲线 + 5 层累计曲线 + 统计卡片 + 区间/horizon/layers 控件)。

5. **删除因子**:`DELETE /api/factors/{id}`。

验证:跟踪返回 `stats.samples` 与区间交易日数一致;`coverage` 覆盖率接近 100%(除停牌/未上市);
分层累计曲线单调性与 `direction` 的预期一致(如 `size_log` 在 2023-2024 小市值占优期
应呈“层1(小市值)在上”)。

## 口径与边界

- 因子值:当日截面计算,缺失(窗口不足/停牌/字段为空)直接剔除,不计 0;
- 前向收益:`close[D+horizon]/close[D]-1`(horizon 默认 5 交易日);
- IC:皮尔逊相关;RankIC:斯皮尔曼秩相关(并列取平均秩);
- 分层:按因子值升序等分 N 层(层 1 = 因子值最小),`layer_cum` 按 `1+单期/horizon` 逐日复合;
- 性能参考:全市场 2 年跟踪约 7s(本地)/ 3s(NAS),按需查询不落盘;
- 边界:未做行业/市值中性化,未做换手与衰减分析(后续可按需扩展);
- 面板与 `/api/factors` 均只读/无鉴权(限可信内网),注册与删除会写湖内 `meta/factors.json`。

## 关联

- 截面数据:`quantd build-cross`(`harness/experience/backtest-performance.md`)
- 面板入口:`harness/workflow/backtest-service.md`(在线文档与管理面板)
- 实现:`internal/factor/`(注册表 / 截面计算 / 跟踪 / HTTP)
