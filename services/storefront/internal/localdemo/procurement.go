package localdemo

import (
	"encoding/json"
	"errors"
	"gptibo/storefront/internal/security"
	"io"
	"net/http"
	"strings"
)

type GoodsTransport struct{ State *State }

func (t GoodsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "local-goods.example.invalid" && r.URL.Host != "delivery.example.invalid" {
		return nil, errors.New("external procurement disabled")
	}
	t.State.Lock()
	defer t.State.Unlock()
	var body map[string]any
	if r.Method == "POST" {
		if json.NewDecoder(io.LimitReader(r.Body, 50000)).Decode(&body) != nil {
			return nil, errors.New("invalid demo purchase")
		}
	}
	path := r.URL.Path
	goods := map[string]any{"goodsId": 1, "name": "Gmail 本机演示账号（不可登录）", "salesPrice": 1.00, "goodsType": 1, "status": 1, "skuType": 0, "stockCount": 100, "rechargeTemplates": []any{}}
	var result any
	switch path {
	case "/api/v3/goods/getDetail":
		result = map[string]any{"code": 1000, "data": goods}
	case "/api/v3/goods/getList":
		result = map[string]any{"code": 1000, "data": map[string]any{"allCount": 1, "allPage": 1, "items": []any{goods}}}
	case "/api/v3/order/create", "/api/v3/order/getDetail":
		merchant, _ := body["outerNumber"].(string)
		if merchant == "" {
			return nil, errors.New("missing demo purchase identity")
		}
		result = map[string]any{"code": 1000, "data": map[string]any{"orderNumber": "LOCAL-GMAIL-" + merchant, "outerNumber": merchant, "status": 3, "count": 1, "money": 1.00, "retMoney": 0, "cards": []any{map[string]any{"cardId": "LOCAL-CARD-" + merchant, "cardNo": "demo-" + merchant[:8] + "@example.invalid", "cardPwd": "LOCAL-DEMO-NOT-A-PASSWORD", "status": 0}}}}
	case "/api/v1/products":
		result = map[string]any{"services": []any{map[string]any{"service_id": "DEMO_PRODUCT_A", "name": "Gemini 演示兑换链接", "price": 0.10, "stock": 100, "availability_state": "available"}, map[string]any{"service_id": "DEMO_PRODUCT_B", "name": "多邻国演示兑换码", "price": 0.10, "stock": 100, "availability_state": "available"}}}
	case "/api/v1/me":
		result = map[string]any{"wallet_balance": 99999}
	case "/api/v1/order":
		key := "goods/" + r.Header.Get("Idempotency-Key")
		if key == "goods/" {
			return nil, errors.New("missing demo receipt")
		}
		if raw := t.State.Receipts[key]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &result)
			break
		}
		id := "LOCAL-GOODS-" + security.ID()
		service, _ := body["service_id"].(string)
		value := "LOCAL-DEMO-CDK-" + id
		if service == "DEMO_PRODUCT_A" {
			value = "https://claim.example.invalid/local-demo/" + id
		}
		order := map[string]any{"order_id": id, "order_status": "success", "service_id": service, "requested_quantity": 1, "delivered_quantity": 1, "charged_amount": 0.10, "refunded_amount": 0, "net_charged_amount": 0.10, "partial_delivery": false, "delivered_products": []string{value}}
		b, _ := json.Marshal(order)
		t.State.Receipts[key] = string(b)
		t.State.Receipts["goods-id/"+id] = string(b)
		result = order
	default:
		if strings.HasPrefix(path, "/api/v1/order/") {
			raw := t.State.Receipts["goods-id/"+strings.TrimPrefix(path, "/api/v1/order/")]
			if raw == "" {
				return nil, errors.New("unknown demo delivery")
			}
			var order any
			_ = json.Unmarshal([]byte(raw), &order)
			result = map[string]any{"order": order}
		} else {
			return nil, errors.New("unsupported demo procurement operation")
		}
	}
	if e := t.State.save(); e != nil {
		return nil, e
	}
	return response(200, result), nil
}
