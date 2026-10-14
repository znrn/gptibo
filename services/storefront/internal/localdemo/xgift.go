package localdemo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type XOrder struct {
	ID        string  `json:"id"`
	Merchant  string  `json:"merchant_order_no"`
	Product   string  `json:"product_code"`
	Business  string  `json:"business"`
	Supply    int64   `json:"supply_minor"`
	Recipient string  `json:"recipient"`
	Status    string  `json:"status"`
	Receipt   *string `json:"receipt"`
	Failure   *string `json:"failure_code"`
	Created   string  `json:"created_at"`
	Updated   string  `json:"updated_at"`
	Stage     string  `json:"stage"`
}
type XTransport struct{ State *State }

func xRecipient(username string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(username))
	return strconv.FormatUint(h.Sum64(), 10)
}
func xDigest(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
func xFinish(o XOrder, fail bool) XOrder {
	o.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	if fail {
		code := "x_price_changed"
		o.Status, o.Stage, o.Failure = "failed", "preflight", &code
	} else {
		receipt := "rcpt_" + xDigest(o.Merchant)[:24]
		o.Status, o.Stage, o.Receipt = "succeeded", "completed", &receipt
	}
	return o
}

// Only the local demo command constructs this transport. There is no network fallback.
func (s XTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "local-x.example.invalid" {
		return nil, errors.New("external X requests disabled")
	}
	s.State.Lock()
	defer s.State.Unlock()
	var b struct {
		Merchant    string `json:"merchant_order_no"`
		Product     string `json:"product_code"`
		Recipient   string `json:"recipient"`
		RecipientID string `json:"confirmed_recipient_id"`
		Expected    int64  `json:"expected_supply_minor"`
	}
	if r.Method == "POST" {
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b) != nil {
			return nil, errors.New("invalid local X request")
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	var data any
	switch {
	case r.Method == "GET" && path == "products":
		data = []map[string]any{{"code": "x-3m", "name": "模拟 X Premium 3 个月", "business": "x", "supply_minor": 1700, "enabled": true, "available": true}, {"code": "x-6m", "name": "模拟 X Premium 6 个月", "business": "x", "supply_minor": 3400, "enabled": true, "available": true}}
	case r.Method == "GET" && path == "balance":
		data = map[string]any{"currency": "CNY", "available_minor": 100000000, "frozen_minor": 0}
	case r.Method == "POST" && path == "preflight":
		data = map[string]any{"recipient": b.Recipient, "recipient_id": xRecipient(b.Recipient), "eligible": b.Recipient != "demo_missing"}
	case r.Method == "POST" && path == "orders":
		supply := int64(1700)
		if b.Product == "x-6m" {
			supply = 3400
		}
		if b.Merchant == "" || b.RecipientID != xRecipient(b.Recipient) || (b.Product != "x-3m" && b.Product != "x-6m") || b.Expected != supply {
			return nil, errors.New("invalid local X order")
		}
		if old, ok := s.State.XOrders[b.Merchant]; ok {
			if old.Product != b.Product || old.Recipient != b.Recipient || old.Supply != b.Expected {
				return nil, errors.New("local X idempotency conflict")
			}
			data = old
		} else {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			o := XOrder{ID: "ord_" + xDigest(b.Merchant)[:32], Merchant: b.Merchant, Product: b.Product, Business: "x", Supply: supply, Recipient: b.Recipient, Status: "running", Created: now, Updated: now, Stage: "processing"}
			s.State.XOrders[b.Merchant] = o
			if e := s.State.save(); e != nil {
				return nil, e
			}
			data = o
		}
	case r.Method == "GET" && strings.HasPrefix(path, "orders/by-merchant/"):
		merchant := strings.TrimPrefix(path, "orders/by-merchant/")
		o, ok := s.State.XOrders[merchant]
		if !ok {
			return response(404, map[string]any{"error": map[string]string{"code": "order_missing"}}), nil
		}
		created, _ := time.Parse(time.RFC3339Nano, o.Created)
		if o.Status == "running" && time.Since(created) >= 5*time.Second {
			fail := o.Recipient == "demo_fail" || o.Recipient == "demo_retry" && !strings.Contains(o.Merchant, ":retry:")
			o = xFinish(o, fail)
			s.State.XOrders[merchant] = o
			if e := s.State.save(); e != nil {
				return nil, e
			}
		}
		data = o
	default:
		return nil, errors.New("unsupported local X request")
	}
	return response(200, map[string]any{"data": data}), nil
}
