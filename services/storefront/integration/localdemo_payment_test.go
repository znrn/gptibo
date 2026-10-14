package integration_test

import (
	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/internal/localdemo"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/security"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func TestLocalDemoPaymentOriginalRefundAndIdempotency(t *testing.T) {
	owner, app, a, base, mail := identityFixture(t)
	admin := login(t, base, mail, "admin", "")
	execSQL(t, owner, `INSERT INTO catalog_plans(id,product_id,name,price_cents) VALUES('plus','chatgpt','Plus',13900)`)
	state, e := localdemo.OpenState(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	p := localdemo.Payment{State: state, Channel: "alipay", Origin: testOrigin}
	ps := &payments.Service{Store: app, Provider: p, Checkout: true, NotifyOrigin: testOrigin}
	h := httpapi.Router(app, "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Options{Origin: testOrigin, Auth: a, Payments: ps, PaymentTesting: true})
	id := object(t, request(h, "POST", "/api/admin/payments/test", map[string]any{"key": security.ID(), "email": "demo@example.test"}, admin), 201)["order"].(map[string]any)["id"].(string)
	path := "/api/admin/payments/test/" + id
	object(t, request(h, "POST", path, map[string]any{"action": "create"}, admin), 200)
	state.Lock()
	state.Payments[id] = "paid"
	state.Unlock()
	paid := object(t, request(h, "POST", path, map[string]any{"action": "query"}, admin), 200)["order"].(map[string]any)
	if paid["paymentStatus"] != "paid" {
		t.Fatal("local payment did not reconcile")
	}
	for range 2 {
		o := object(t, request(h, "POST", path, map[string]any{"action": "refund"}, admin), 200)["order"].(map[string]any)
		if o["refundedCents"] != float64(1) {
			t.Fatal("local refund ledger not confirmed")
		}
	}
}
