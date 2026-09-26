package tsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quant-core/internal/lake"
	"quant-core/internal/schema"
)

func setupLake(t *testing.T) *lake.Lake {
	t.Helper()
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	l := lake.New(t.TempDir(), reg)
	ds, _ := reg.Get("bars_daily")

	row := func(code string, days int64, close float64) []schema.Value {
		out := make([]schema.Value, len(ds.Fields))
		for i := range out {
			out[i] = schema.NullValue()
		}
		set := func(name string, v schema.Value) {
			i, _ := ds.FieldIndex(name)
			out[i] = v
		}
		set("ts_code", schema.Str(code))
		set("trade_date", schema.Date(days))
		set("open", schema.Float(close - 0.1))
		set("close", schema.Float(close))
		set("vol", schema.Float(1000))
		set("amount", schema.Float(close * 100))
		set("pe", schema.Float(10.5))
		return out
	}
	dir := filepath.Join(l.Dir(ds), "year=2023")
	pw, err := lake.NewPartWriter(ds, dir)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	rows := [][]schema.Value{
		row("600000.SH", 19361, 7.31),
		row("600000.SH", 19362, 7.35),
		row("000001.SZ", 19361, 12.5),
	}
	for _, r := range rows {
		if err := pw.WriteRow(r); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := pw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return l
}

func post(t *testing.T, srv *Server, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestDailyAPI(t *testing.T) {
	srv := NewServer(setupLake(t))
	resp := post(t, srv, `{"api_name":"daily","params":{"ts_code":"600000.SH","start_date":"20230104","end_date":"20230110"},"fields":"ts_code,trade_date,close"}`)
	if code := resp["code"].(float64); code != 0 {
		t.Fatalf("code=%v msg=%v", code, resp["msg"])
	}
	data := resp["data"].(map[string]any)
	fields := data["fields"].([]any)
	if len(fields) != 3 || fields[0] != "ts_code" || fields[2] != "close" {
		t.Fatalf("fields = %v", fields)
	}
	items := data["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	first := items[0].([]any)
	if first[1] != "20230104" {
		t.Errorf("trade_date = %v, want 20230104", first[1])
	}
	if first[2].(float64) != 7.31 {
		t.Errorf("close = %v", first[2])
	}
	if data["has_more"].(bool) {
		t.Error("has_more should be false")
	}
}

func TestDailyBasicAPI(t *testing.T) {
	srv := NewServer(setupLake(t))
	resp := post(t, srv, `{"api_name":"daily_basic","params":{"ts_code":"600000.SH","trade_date":"20230104"},"fields":"ts_code,trade_date,close,pe"}`)
	data := resp["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d (%v)", len(items), resp)
	}
	row := items[0].([]any)
	if row[3].(float64) != 10.5 {
		t.Errorf("pe = %v", row[3])
	}
}

func TestStkMinsUnitTransform(t *testing.T) {
	reg, err := schema.Load(filepath.Join("..", "..", "schemas", "datasets.yaml"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	l := lake.New(t.TempDir(), reg)
	ds, _ := reg.Get("bars_1m")

	// 内部存储:vol=手、amount=千元
	micros := schema.MicrosFromTime(time.Date(2024, 1, 2, 9, 31, 0, 0, time.UTC))
	row := make([]schema.Value, len(ds.Fields))
	for i := range row {
		row[i] = schema.NullValue()
	}
	set := func(name string, v schema.Value) {
		i, _ := ds.FieldIndex(name)
		row[i] = v
	}
	set("ts_code", schema.Str("600000.SH"))
	set("trade_time", schema.Timestamp(micros))
	set("open", schema.Float(10.0))
	set("high", schema.Float(10.1))
	set("low", schema.Float(9.9))
	set("close", schema.Float(10.05))
	set("vol", schema.Float(1234.5))   // 手
	set("amount", schema.Float(1234.5)) // 千元

	dir := filepath.Join(l.Dir(ds), "market=hs", "year=2024", "month=01")
	pw, err := lake.NewPartWriter(ds, dir)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if err := pw.WriteRow(row); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	srv := NewServer(l)
	resp := post(t, srv, `{"api_name":"stk_mins","params":{"ts_code":"600000.SH","freq":"1min","start_date":"2024-01-02 09:00:00","end_date":"2024-01-02 15:30:00"}}`)
	data := resp["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d (%v)", len(items), resp)
	}
	rowOut := items[0].([]any)
	// fields: ts_code, trade_time, open, high, low, close, vol, amount
	if got := rowOut[6].(float64); got != 123450 { // 手 -> 股
		t.Errorf("vol = %v, want 123450 (100x)", got)
	}
	if got := rowOut[7].(float64); got != 1234500 { // 千元 -> 元
		t.Errorf("amount = %v, want 1234500 (1000x)", got)
	}
}

func TestUnknownAPIFails(t *testing.T) {
	srv := NewServer(setupLake(t))
	resp := post(t, srv, `{"api_name":"not_exists","params":{}}`)
	if code := resp["code"].(float64); code == 0 {
		t.Fatal("expected non-zero code for unknown api")
	}
	if msg, _ := resp["msg"].(string); !strings.Contains(msg, "unknown api_name") {
		t.Fatalf("msg = %v", resp["msg"])
	}
}

func TestFieldWhitelist(t *testing.T) {
	srv := NewServer(setupLake(t))
	resp := post(t, srv, `{"api_name":"daily","params":{"ts_code":"600000.SH"},"fields":"ts_code,pe_ttm"}`)
	if code := resp["code"].(float64); code == 0 {
		t.Fatal("daily api must not expose pe_ttm through fields whitelist")
	}
}

func TestTokenValidation(t *testing.T) {
	lakeInst := setupLake(t)
	srv := NewServer(lakeInst)
	srv.Tokens = []string{"secret"}
	resp := post(t, srv, `{"api_name":"daily","params":{"ts_code":"600000.SH"},"token":"wrong"}`)
	if code := resp["code"].(float64); code == 0 {
		t.Fatal("expected token rejection")
	}
	resp = post(t, srv, `{"api_name":"daily","params":{"ts_code":"600000.SH"},"token":"secret"}`)
	if code := resp["code"].(float64); code != 0 {
		t.Fatalf("valid token rejected: %v", resp)
	}
}

func TestLimitAndHasMore(t *testing.T) {
	srv := NewServer(setupLake(t))
	resp := post(t, srv, `{"api_name":"daily","params":{"ts_code":"600000.SH","limit":"1"},"fields":"ts_code,trade_date"}`)
	data := resp["data"].(map[string]any)
	if n := len(data["items"].([]any)); n != 1 {
		t.Fatalf("want 1 item, got %d", n)
	}
	if !data["has_more"].(bool) {
		t.Error("has_more should be true when more rows exist")
	}
}
