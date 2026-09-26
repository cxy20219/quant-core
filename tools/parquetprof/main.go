package main

import (
	"fmt"
	"os"
	"time"

	"github.com/parquet-go/parquet-go"
)

func main() {
	path := os.Args[1]
	// 热身
	for i := 0; i < 3; i++ {
		fh, _ := os.Open(path)
		st, _ := fh.Stat()
		f, err := parquet.OpenFile(fh, st.Size())
		if err != nil {
			panic(err)
		}
		fmt.Printf("run %d: rows=%d rowgroups=%d footer_size=%d\n", i, f.NumRows(), len(f.RowGroups()), f.Size())
		// 计时 OpenFile
		start := time.Now()
		for j := 0; j < 20; j++ {
			fh2, _ := os.Open(path)
			st2, _ := fh2.Stat()
			_, _ = parquet.OpenFile(fh2, st2.Size())
			fh2.Close()
		}
		fmt.Printf("  OpenFile avg: %.2f ms\n", float64(time.Since(start).Microseconds())/20.0/1000.0)

		// 计时读取单个行组(含列索引)
		start = time.Now()
		for j := 0; j < 20; j++ {
			rg := f.RowGroups()[33]
			idx, _ := rg.ColumnChunks()[21].ColumnIndex()
			_ = idx.MinValue(0)
		}
		fmt.Printf("  ColumnIndex(1 col) avg: %.2f ms\n", float64(time.Since(start).Microseconds())/20.0/1000.0)

		start = time.Now()
		for j := 0; j < 5; j++ {
			rg := f.RowGroups()[33]
			for k := range rg.ColumnChunks() {
				idx, err := rg.ColumnChunks()[k].ColumnIndex()
				if err == nil && idx != nil && idx.NumPages() > 0 {
					_ = idx.MinValue(0)
					_ = idx.MaxValue(0)
				}
			}
		}
		fmt.Printf("  ColumnIndex(all 26 cols) avg: %.2f ms\n", float64(time.Since(start).Microseconds())/5.0/1000.0)
		fh.Close()
	}
}
