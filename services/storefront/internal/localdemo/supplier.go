package localdemo

import (
	"context"
	"encoding/json"
	"errors"
	"gptibo/storefront/internal/recharge"
	"gptibo/storefront/internal/security"
	"io"
	"net/http"
	"strings"
	"time"
)

type Supplier struct{ State *State }

func response(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b)))}
}

// The transport implements the supplier contract in process. Unknown hosts and
// operations fail closed; there is deliberately no network transport fallback.
func (s Supplier) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "local-supplier.example.invalid" {
		return nil, errors.New("external requests disabled in local demo")
	}
	s.State.Lock()
	defer s.State.Unlock()
	var body map[string]any
	if r.Method == "POST" && r.Body != nil {
		if json.NewDecoder(io.LimitReader(r.Body, 50000)).Decode(&body) != nil {
			return nil, errors.New("invalid demo supplier request")
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	key := r.Header.Get("Idempotency-Key")
	if r.Method == "POST" && key != "" && s.State.Receipts["rpc/"+path+"/"+key] != "" {
		var v any
		_ = json.Unmarshal([]byte(s.State.Receipts["rpc/"+path+"/"+key]), &v)
		return response(200, map[string]any{"data": v}), nil
	}
	var data any
	switch {
	case path == "products" && r.Method == "GET":
		p := []recharge.Product{}
		for _, id := range []string{"plus", "pro-5x", "pro-20x", "pro-25x"} {
			p = append(p, recharge.Product{Code: id, Supply: "1.00", MaxSale: "99999.00", Currency: "CNY", Quantity: 1, Available: true, Modes: []string{"auto_recharge"}})
		}
		data = p
	case path == "payment-methods" && r.Method == "GET":
		data = []map[string]string{{"code": "alipay"}}
	case path == "orders" && r.Method == "POST":
		merchant, _ := body["merchant_order_no"].(string)
		product, _ := body["product_code"].(string)
		sale, _ := body["sale_amount"].(string)
		amount, e := recharge.Money(sale)
		if e != nil || merchant == "" || amount < 100 {
			return nil, errors.New("invalid local order")
		}
		o := recharge.RemoteOrder{ID: "LOCAL-" + merchant, Merchant: merchant, Product: product, Quantity: 1, Currency: "CNY", Collection: "platform_collect", Scope: "retail", Delivery: "auto_recharge", Channel: "alipay", Sale: sale, Supply: "1.00", Margin: recharge.Amount(amount - 100), Payment: "pending", Refunded: "0.00", Expires: time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano)}
		s.State.Orders[o.ID] = o
		data = o
	case strings.HasPrefix(path, "orders/") && strings.HasSuffix(path, "/fulfillments") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "orders/"), "/fulfillments")
		list := []map[string]any{}
		for _, a := range s.State.Attempts {
			if a.Order == id {
				list = append(list, map[string]any{"fulfillment_id": a.ID, "order_id": id, "attempt_no": a.Attempt})
			}
		}
		data = list
	case strings.HasPrefix(path, "orders/") && strings.HasSuffix(path, "/refunds") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "orders/"), "/refunds")
		o, ok := s.State.Orders[id]
		if !ok {
			return response(404, map[string]any{"error": map[string]string{"code": "order_not_found"}}), nil
		}
		amount, _ := body["amount"].(string)
		merchant, _ := body["merchant_refund_no"].(string)
		c, e := recharge.Money(amount)
		sale, _ := recharge.Money(o.Sale)
		old, _ := recharge.Money(o.Refunded)
		if e != nil || c <= 0 || c+old > sale {
			return nil, errors.New("invalid local refund")
		}
		o.Refunded = recharge.Amount(c + old)
		o.Margin = signedAmount(sale - c - old - 100)
		o.Payment = "partially_refunded"
		if c+old == sale {
			o.Payment = "refunded"
		}
		s.State.Orders[id] = o
		f := recharge.RemoteRefund{ID: "LOCAL-RF-" + security.ID(), Order: id, Merchant: merchant, Amount: amount, Status: "succeeded"}
		s.State.Refunds[f.ID] = f
		data = f
	case strings.HasPrefix(path, "orders/") && r.Method == "GET":
		o, ok := s.State.Orders[strings.TrimPrefix(path, "orders/")]
		if !ok {
			return response(404, map[string]any{"error": map[string]string{"code": "order_not_found"}}), nil
		}
		data = o
	case path == "redemptions" && r.Method == "POST":
		id, _ := body["order_id"].(string)
		if _, ok := s.State.Orders[id]; !ok {
			return nil, errors.New("unknown local order")
		}
		n := int64(1)
		for _, old := range s.State.Attempts {
			if old.Order == id && old.Attempt != nil {
				n = max(n, *old.Attempt+1)
			}
		}
		yes := true
		version := int64(1)
		stage := "processing"
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		a := recharge.Redemption{ID: "LOCAL-RD-" + security.ID(), Order: id, Status: "running", Fallback: &yes, Version: &version, Attempt: &n, Stage: &stage, Updated: &stamp}
		s.State.Attempts[a.ID] = a
		data = a
	case strings.HasPrefix(path, "redemptions/") && r.Method == "GET":
		a, ok := s.State.Attempts[strings.TrimPrefix(path, "redemptions/")]
		if !ok {
			return nil, errors.New("unknown local attempt")
		}
		data = a
	case strings.HasPrefix(path, "refunds/") && r.Method == "GET":
		data = s.State.Refunds[strings.TrimPrefix(path, "refunds/")]
	default:
		return nil, errors.New("unsupported local supplier operation")
	}
	if r.Method == "POST" && key != "" {
		b, _ := json.Marshal(data)
		s.State.Receipts["rpc/"+path+"/"+key] = string(b)
	}
	if e := s.State.save(); e != nil {
		return nil, e
	}
	return response(200, map[string]any{"data": data}), nil
}

func signedAmount(cents int64) string {
	if cents < 0 {
		return "-" + recharge.Amount(-cents)
	}
	return recharge.Amount(cents)
}

type SessionChecker struct{}

func (SessionChecker) Check(_ context.Context, _ recharge.Credential, plan string) (recharge.PrecheckResult, error) {
	return recharge.PrecheckResult{State: "eligible", Code: "eligible", Target: plan, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

// Transition only changes the fake upstream. The original reconciliation logic
// performs all order, attempt, refund and ledger updates in PostgreSQL.
func (s Supplier) Transition(merchant, action string) error {
	s.State.Lock()
	defer s.State.Unlock()
	id := "LOCAL-" + merchant
	o, ok := s.State.Orders[id]
	if !ok {
		return errors.New("not a local recharge order")
	}
	if action == "pay" {
		if o.Payment != "pending" {
			return errors.New("order is no longer awaiting payment")
		}
		o.Payment = "paid"
	} else {
		if o.Payment != "paid" {
			return errors.New("order must be paid first")
		}
		var latest *recharge.Redemption
		for _, a := range s.State.Attempts {
			if a.Order == id && (latest == nil || *a.Attempt > *latest.Attempt) {
				v := a
				latest = &v
			}
		}
		if latest == nil {
			return errors.New("submit demo credentials before changing fulfillment")
		}
		if latest.Status != "running" && latest.Status != "queued" {
			return errors.New("attempt already completed; use the original retry workflow")
		}
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		v := *latest.Version + 1
		latest.Version = &v
		latest.Updated = &stamp
		latest.Finished = &stamp
		switch action {
		case "success":
			latest.Status = "succeeded"
			o.Fulfillment = "succeeded"
		case "fail":
			latest.Status = "failed"
			latest.Retry = true
			latest.Next = "resubmit"
			o.Fulfillment = "failed"
		default:
			return errors.New("unsupported simulation")
		}
		s.State.Attempts[latest.ID] = *latest
	}
	s.State.Orders[id] = o
	return s.State.save()
}
