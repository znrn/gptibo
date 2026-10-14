package localdemo

import (
	"context"
	"encoding/json"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/recharge"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{Database: "gptibo_demo", OwnerPassword: strings.Repeat("a", 64), RuntimePassword: strings.Repeat("b", 64), Secret: strings.Repeat("c", 64), LaunchKey: strings.Repeat("d", 64), TOTP: strings.Repeat("A", 32), Origin: "http://localhost:8788", Listen: "127.0.0.1:8788", State: filepath.Join(t.TempDir(), "state.json"), Dist: t.TempDir()}
}
func TestLocalBoundary(t *testing.T) {
	c := testConfig(t)
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, alter := range []func(*Config){func(c *Config) { c.Listen = "0.0.0.0:8788" }, func(c *Config) { c.Database = "production" }, func(c *Config) { c.Origin = "https://public.example.com" }, func(c *Config) { c.LaunchKey = "" }} {
		bad := c
		alter(&bad)
		if bad.Validate() == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
	s := &Server{Config: c}
	for _, tc := range []struct {
		host, remote, origin, method string
		ok                           bool
	}{{"localhost:8788", "127.0.0.1:1234", "", "GET", true}, {"localhost:8788", "127.0.0.1:1234", c.Origin, "POST", true}, {"localhost:8788", "192.168.1.1:1234", c.Origin, "GET", false}, {"evil.example:8788", "127.0.0.1:1234", "", "GET", false}, {"localhost:8788", "127.0.0.1:1234", "http://evil.example", "POST", false}, {"localhost:8788", "127.0.0.1:1234", "", "POST", false}} {
		r := httptest.NewRequest(tc.method, c.Origin, nil)
		r.Host = tc.host
		r.RemoteAddr = tc.remote
		r.Header.Set("Origin", tc.origin)
		if s.allowed(r) != tc.ok {
			t.Fatalf("boundary mismatch: %+v", tc)
		}
	}
	r := httptest.NewRequest("GET", c.Origin+"/local-demo/start?key=bad", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("bootstrap accepted invalid key")
	}
	r = httptest.NewRequest("GET", c.Origin+"/api/local-demo/status", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("mailbox exposed without admin session")
	}
}
func TestSupplierPersistenceAndLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state, e := OpenState(path)
	if e != nil {
		t.Fatal(e)
	}
	sim := Supplier{state}
	client, _ := recharge.NewClient("https://local-supplier.example.invalid/v1", "demo", "demo", "demo")
	client.HTTP = &http.Client{Transport: sim}
	var products []recharge.Product
	if e = client.Request(context.Background(), "/products", nil, "", &products); e != nil || len(products) != 4 {
		t.Fatal("GET product contract failed", e)
	}
	body := recharge.Raw(map[string]any{"merchant_order_no": "YMX-LOCAL", "product_code": "plus", "sale_amount": "139.00"})
	var order recharge.RemoteOrder
	if e = client.Request(context.Background(), "/orders", &body, "create-key", &order); e != nil {
		t.Fatal(e)
	}
	if e = sim.Transition("YMX-LOCAL", "success"); e == nil {
		t.Fatal("unpaid fulfillment accepted")
	}
	if e = sim.Transition("YMX-LOCAL", "pay"); e != nil {
		t.Fatal(e)
	}
	body = `{"order_id":"LOCAL-YMX-LOCAL"}`
	var a recharge.Redemption
	if e = client.Request(context.Background(), "/redemptions", &body, "attempt-key", &a); e != nil {
		t.Fatal(e)
	}
	if e = sim.Transition("YMX-LOCAL", "fail"); e != nil {
		t.Fatal(e)
	}
	if e = sim.Transition("YMX-LOCAL", "success"); e == nil {
		t.Fatal("terminal attempt overwritten")
	}
	loaded, e := OpenState(path)
	if e != nil {
		t.Fatal(e)
	}
	client.HTTP.Transport = Supplier{loaded}
	var retry recharge.Redemption
	if e = client.Request(context.Background(), "/redemptions", &body, "attempt-key", &retry); e != nil || retry.ID != a.ID || len(loaded.Attempts) != 1 {
		t.Fatal("idempotency lost after restart", e)
	}
	if e = client.Request(context.Background(), "/redemptions", &body, "attempt-key-2", &retry); e != nil || *retry.Attempt != 2 {
		t.Fatal("second attempt contract failed", e)
	}
	r := httptest.NewRequest("GET", "https://real-supplier.example.com/v1/products", nil)
	if _, e = sim.RoundTrip(r); e == nil {
		t.Fatal("external supplier accepted")
	}
}
func TestLocalMailAndPayment(t *testing.T) {
	s, _ := OpenState(filepath.Join(t.TempDir(), "state.json"))
	m := Mailbox{s}
	ctx := context.Background()
	if e := m.SendCode(ctx, "partner", "demo@example.invalid", "123456", "challenge"); e != nil {
		t.Fatal(e)
	}
	_ = m.SendCode(ctx, "partner", "demo@example.invalid", "123456", "challenge")
	if len(s.Letters) != 1 || m.Code("challenge") != "123456" {
		t.Fatal("mail idempotency failed")
	}
	p := Payment{s, "alipay", "http://localhost:8788"}
	o := payments.Order{ID: "demo", Amount: 100}
	if _, e := p.Create(ctx, o, ""); e != nil {
		t.Fatal(e)
	}
	s.Payments[o.ID] = "paid"
	if closed, _ := p.Close(ctx, o); closed {
		t.Fatal("paid transaction closed")
	}
	res, _ := p.Query(ctx, o)
	if res.Status != "paid" || !strings.HasPrefix(res.TradeNo, "LOCAL-DEMO-") {
		t.Fatal("invalid local payment")
	}
	o.TradeNo = res.TradeNo
	refund := payments.Refund{ID: "case", Amount: 100}
	if before, _ := p.QueryRefund(ctx, o, refund); before.Confirmed {
		t.Fatal("unsubmitted refund reported paid")
	}
	if got, e := p.Refund(ctx, o, refund); e != nil || !got.Confirmed || got.TradeNo != res.TradeNo {
		t.Fatal("refund lost original trade identity", e)
	}
	if got, _ := p.QueryRefund(ctx, o, refund); !got.Confirmed {
		t.Fatal("refund receipt missing")
	}
	refund.Amount = 99
	if _, e := p.Refund(ctx, o, refund); e == nil {
		t.Fatal("conflicting refund accepted")
	}
	if _, e := p.Notification(httptest.NewRequest("POST", "http://localhost/notify", nil)); e == nil {
		t.Fatal("external callback accepted")
	}
}
func TestProcurementIdempotency(t *testing.T) {
	s, _ := OpenState(filepath.Join(t.TempDir(), "state.json"))
	p := GoodsTransport{s}
	request := func() *http.Response {
		r := httptest.NewRequest("POST", "https://delivery.example.invalid/api/v1/order", strings.NewReader(`{"service_id":"DEMO_PRODUCT_A"}`))
		r.Header.Set("Idempotency-Key", "same")
		res, e := p.RoundTrip(r)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	a, _ := io.ReadAll(request().Body)
	b, _ := io.ReadAll(request().Body)
	if string(a) != string(b) {
		t.Fatal("duplicate procurement created")
	}
	var order map[string]any
	if json.Unmarshal(a, &order) != nil || order["order_status"] != "success" {
		t.Fatal("invalid procurement contract")
	}
}

func TestSignedSupplierMargin(t *testing.T) {
	if signedAmount(-50) != "-0.50" || signedAmount(-100) != "-1.00" {
		t.Fatal("negative refund margin malformed")
	}
}
