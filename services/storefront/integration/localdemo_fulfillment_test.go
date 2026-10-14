package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"gptibo/storefront/internal/httpapi"
	"gptibo/storefront/internal/localdemo"
	"gptibo/storefront/internal/payments"
	"gptibo/storefront/internal/security"
	"gptibo/storefront/internal/xgift"
)

func TestLocalDemoSMSAndXFulfillment(t *testing.T) {
	owner, app, a, base, mail := identityFixture(t)
	admin := login(t, base, mail, "admin", "")
	execSQL(t, owner, `INSERT INTO catalog_plans(id,product_id,name,price_cents,enabled) VALUES('codex-sms-once','codex-sms','SMS',1000,true),('x-premium-3m','x-premium','X3',1700,true),('x-premium-6m','x-premium','X6',3400,true)`)
	state, e := localdemo.OpenState(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	p := localdemo.Payment{State: state, Channel: "alipay", Origin: testOrigin}
	ps := &payments.Service{Store: app, Provider: p, Checkout: true, NotifyOrigin: testOrigin}
	xc, e := xgift.NewClient("https://local-x.example.invalid/v1", "platform", "", "demo", "demo")
	if e != nil {
		t.Fatal(e)
	}
	xc.HTTP.Transport = localdemo.XTransport{State: state}
	xg := &xgift.Service{Store: app, Auth: a, Backend: "platform", Clients: map[string]*xgift.Client{"platform": xc}, Checkout: true, PaymentReady: true}
	sms := localdemo.SMS{Store: app}
	h := httpapi.Router(app, "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Options{Origin: testOrigin, Auth: a, Payments: ps, XGift: xg, Fulfillment: sms})
	buyer := &http.Cookie{Name: "__Host-sf_buyer", Value: security.Token()}
	read := func(id string) map[string]any {
		return object(t, request(h, "GET", "/api/orders/"+id, nil, buyer), 200)["order"].(map[string]any)
	}
	create := func(plan, username string) string {
		price := 1000
		if plan == "x-premium-3m" {
			price = 1700
		}
		if plan == "x-premium-6m" {
			price = 3400
		}
		in := map[string]any{"plan": plan, "email": "buyer@example.test", "key": security.ID(), "expectedPrice": price, "paymentMethod": "alipay", "expectedFeeBps": 0}
		if username != "" {
			pre := object(t, request(h, "POST", "/api/orders/xgift/eligibility", map[string]any{"plan": plan, "username": username}, buyer), 200)
			in["username"] = username
			in["preflightToken"] = pre["token"]
		}
		return object(t, request(h, "POST", "/api/orders", in, buyer), 201)["order"].(map[string]any)["id"].(string)
	}
	pay := func(id string) {
		object(t, request(h, "POST", "/api/orders/"+id+"/payment", map[string]any{"action": "create"}, buyer), 200)
		state.Lock()
		state.Payments[id] = "paid"
		state.Unlock()
		object(t, request(h, "POST", "/api/orders/"+id+"/payment", map[string]any{"action": "query"}, buyer), 200)
	}
	action := func(id, action string, status int) {
		o := read(id)
		object(t, request(h, "POST", "/api/orders/"+id+"/demo-actions", map[string]any{"action": action, "version": o["version"]}, buyer), status)
	}
	refund := func(id string, amount int) {
		o := read(id)
		key := security.ID()
		object(t, request(h, "POST", "/api/orders/"+id+"/refunds", map[string]any{"key": key, "reason": "本机模拟售后", "version": o["version"]}, buyer), 201)
		var rid string
		if e := owner.DB.QueryRow(context.Background(), `SELECT id FROM refund_cases WHERE request_key=$1`, key).Scan(&rid); e != nil {
			t.Fatal(e)
		}
		object(t, request(h, "POST", "/api/admin/refunds/"+rid, map[string]any{"action": "approve", "version": 0, "amountCents": amount, "note": "模拟退款审批"}, admin), 200)
		object(t, request(h, "POST", "/api/admin/refunds/"+rid, map[string]any{"action": "execute-refund", "version": 1, "note": "模拟原路退款"}, admin), 200)
		if read(id)["refundedCents"] != float64(amount) {
			t.Fatal("refund ledger mismatch")
		}
	}
	t.Run("sms-timeout-retry-refund-and-guards", func(t *testing.T) {
		id := create("codex-sms-once", "")
		action(id, "acquire-phone", 409)
		action(id, "pay", 409)
		pay(id)
		action(id, "acquire-phone", 200)
		o := read(id)
		oldVersion := o["version"]
		object(t, request(h, "POST", "/api/orders/"+id+"/demo-actions", map[string]any{"action": "submit-success", "version": oldVersion}, &http.Cookie{Name: buyer.Name, Value: security.Token()}), 404)
		action(id, "submit-failed", 200)
		object(t, request(h, "POST", "/api/orders/"+id+"/demo-actions", map[string]any{"action": "submit-success", "version": oldVersion}, buyer), 409)
		execSQL(t, owner, `UPDATE orders SET started_at=1 WHERE id=$1`, id)
		if e := sms.Reconcile(context.Background()); e != nil {
			t.Fatal(e)
		}
		if read(id)["status"] != "failed" {
			t.Fatal("SMS timeout not reconciled")
		}
		action(id, "retry", 200)
		action(id, "submit-success", 200)
		execSQL(t, owner, `UPDATE orders SET started_at=1,refund_hold=1 WHERE id=$1`, id)
		if e := sms.Reconcile(context.Background()); e != nil {
			t.Fatal(e)
		}
		if read(id)["status"] != "processing" {
			t.Fatal("delivery ignored refund hold")
		}
		execSQL(t, owner, `UPDATE orders SET refund_hold=0 WHERE id=$1`, id)
		if e := sms.Reconcile(context.Background()); e != nil {
			t.Fatal(e)
		}
		if read(id)["sms"].(map[string]any)["code"] != "123456" {
			t.Fatal("SMS result missing")
		}
		action(id, "retry", 409)
		refund(id, 1000)
		action(id, "submit-success", 409)
		if read(id)["sms"].(map[string]any)["code"] != nil {
			t.Fatal("refunded SMS exposed code")
		}
	})
	t.Run("x-success-recovery-and-refund", func(t *testing.T) {
		pre := object(t, request(h, "POST", "/api/orders/xgift/eligibility", map[string]any{"plan": "x-premium-3m", "username": "demo_missing"}, buyer), 200)
		if pre["eligible"] != false {
			t.Fatal("ineligible mock accepted")
		}
		for _, tc := range []struct {
			plan, user string
			amount     int
		}{{"x-premium-3m", "demo_ok", 1700}, {"x-premium-6m", "demo_retry", 3400}, {"x-premium-3m", "demo_fail", 1700}} {
			id := create(tc.plan, tc.user)
			if e := xg.Fulfill(context.Background(), id); e != nil {
				t.Fatal(e)
			}
			state.Lock()
			n := len(state.XOrders)
			state.Unlock()
			pay(id)
			finish := func() {
				execSQL(t, owner, `UPDATE xgift_purchases SET next_check=0 WHERE order_id=$1`, id)
				if e := xg.Fulfill(context.Background(), id); e != nil {
					t.Fatal(e)
				}
				state.Lock()
				for k, o := range state.XOrders {
					if o.Merchant == id || o.Merchant == id+":retry:1" {
						o.Created = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
						state.XOrders[k] = o
					}
				}
				state.Unlock()
				execSQL(t, owner, `UPDATE xgift_purchases SET next_check=0 WHERE order_id=$1`, id)
				if e := xg.Fulfill(context.Background(), id); e != nil {
					t.Fatal(e)
				}
			}
			finish()
			if tc.user == "demo_retry" {
				o := read(id)
				if _, ok := o["xgift"].(map[string]any)["recoveryAvailable"]; ok {
					t.Fatal("admin recovery capability leaked to buyer")
				}
				adminView := object(t, request(h, "GET", "/api/admin/orders/"+id, nil, admin), 200)["order"].(map[string]any)
				if adminView["xgift"].(map[string]any)["recoveryAvailable"] != true {
					t.Fatal("price-change recovery unavailable in admin DTO")
				}
				object(t, request(h, "POST", "/api/admin/orders/"+id+"/actions", map[string]any{"action": "xgift-recovery", "version": o["version"], "note": "模拟补发"}, admin), 200)
				object(t, request(h, "POST", "/api/admin/orders/"+id+"/actions", map[string]any{"action": "xgift-recovery", "version": o["version"], "note": "重复请求"}, admin), 409)
				finish()
			}
			expected := "success"
			if tc.user == "demo_fail" {
				expected = "failed"
			}
			if read(id)["status"] != expected {
				t.Fatal("unexpected X result", read(id))
			}
			if e := xg.Fulfill(context.Background(), id); e != nil {
				t.Fatal(e)
			}
			state.Lock()
			count := len(state.XOrders)
			state.Unlock()
			want := n + 1
			if tc.user == "demo_retry" {
				want++
			}
			if count != want {
				t.Fatal("duplicate X delivery")
			}
			if tc.user == "demo_fail" {
				refund(id, tc.amount)
			}
		}
	})
	// Without an injected adapter the production router keeps SMS closed.
	plain := httpapi.Router(app, "fixture", slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.Options{Origin: testOrigin, Auth: a, Payments: ps})
	object(t, request(plain, "POST", "/api/orders", map[string]any{"plan": "codex-sms-once", "email": "buyer@example.test", "key": security.ID()}, buyer), 409)
}
