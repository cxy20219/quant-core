package main

import (
	"fmt"

	"quant-core/internal/lake"
	"quant-core/internal/query"
	"quant-core/internal/schema"
)

func main() {
	reg, _ := schema.Load("schemas/datasets.yaml")
	l := lake.New(`E:\AI-work\quant-core\lake-test`, reg)
	ds, _ := reg.Get("bars_daily")
	tsCodeIdx, _ := ds.FieldIndex("ts_code")
	dateIdx, _ := ds.FieldIndex("trade_date")
	day, _ := schema.ParseDate("20230104")

	sc := query.NewScanner(l)
	cur, err := sc.Open(query.Request{
		Dataset: "bars_daily",
		Filter: &query.Filter{Preds: []query.Predicate{
			query.In(tsCodeIdx, schema.Str("600000.SH")),
			query.Eq(dateIdx, schema.Date(day)),
		}},
		Columns: []string{"ts_code", "trade_date", "close", "pe", "pb", "total_mv"},
		Limit:   5,
	})
	if err != nil {
		panic(err)
	}
	defer cur.Close()
	for cur.Next() {
		row := cur.Row()
		fmt.Printf("row: %v %v close=%v pe=%v pb=%v mv=%v\n",
			row[0].S, schema.FormatDate(row[1].I), row[2].F, row[3], row[4].F, row[5].F)
	}
	fmt.Println("err:", cur.Err(), "stats:", cur.Stats())
}
