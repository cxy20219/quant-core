"""旧湖(quant-data/quant-store)与新湖逐分区行数对账。

只读 parquet metadata,不加载数据,适合在迁移前后快速核对。

用法:
    python tools/verify_migration.py --src E:\\AI-work\\quant-data\\quant-store\\lake --dst D:\\quant-lake
"""

from __future__ import annotations

import argparse
import glob
import os
import sys

import pyarrow.parquet as pq


def files(root: str) -> list[str]:
    return sorted(glob.glob(os.path.join(root, "**", "*.parquet"), recursive=True))


def row_count(paths: list[str]) -> int:
    total = 0
    for path in paths:
        total += pq.ParquetFile(path).metadata.num_rows
    return total


def partition_of(path: str, root: str) -> str:
    rel = os.path.relpath(path, root)
    parts = [p for p in rel.split(os.sep)[:-1] if "=" in p]
    return "/".join(parts) if parts else ""


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--src", required=True, help="旧湖 parquet 根目录")
    parser.add_argument("--dst", required=True, help="新湖根目录")
    parser.add_argument("--dataset", action="append", help="只核对指定数据集(可重复)")
    args = parser.parse_args()

    src_root = os.path.join(args.src)
    dst_root = os.path.join(args.dst, "canonical")

    datasets = args.dataset or ["bars_daily", "bars_1m", "adj_factor", "corporate_actions"]
    failed = False
    for ds in datasets:
        src_files = files(os.path.join(src_root, ds))
        dst_files = files(os.path.join(dst_root, ds))
        src_rows = row_count(src_files)
        dst_rows = row_count(dst_files)
        # 分区级对账
        src_by_part: dict[str, int] = {}
        for path in src_files:
            key = partition_of(path, os.path.join(src_root, ds))
            src_by_part[key] = src_by_part.get(key, 0) + pq.ParquetFile(path).metadata.num_rows
        dst_by_part: dict[str, int] = {}
        for path in dst_files:
            key = partition_of(path, os.path.join(dst_root, ds))
            dst_by_part[key] = dst_by_part.get(key, 0) + pq.ParquetFile(path).metadata.num_rows

        status = "OK" if src_rows == dst_rows else "MISMATCH"
        if src_rows != dst_rows:
            failed = True
        print(f"[{status}] {ds}: src={src_rows:,} dst={dst_rows:,} "
              f"files={len(src_files)}->{len(dst_files)} partitions={len(src_by_part)}->{len(dst_by_part)}")

        only_src = sorted(set(src_by_part) - set(dst_by_part))
        only_dst = sorted(set(dst_by_part) - set(src_by_part))
        mismatch = sorted(k for k in set(src_by_part) & set(dst_by_part) if src_by_part[k] != dst_by_part[k])
        if only_src:
            failed = True
            print(f"    missing partitions: {only_src[:10]}{' ...' if len(only_src) > 10 else ''}")
        if only_dst:
            print(f"    extra partitions:   {only_dst[:10]}{' ...' if len(only_dst) > 10 else ''}")
        if mismatch:
            failed = True
            for k in mismatch[:5]:
                print(f"    partition {k}: src={src_by_part[k]:,} dst={dst_by_part[k]:,}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
