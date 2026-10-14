package localdemo

import (
	"context"
	"errors"
	"gptibo/storefront/internal/payments"
	"net/http"
	"strconv"
)

type Payment struct {
	State           *State
	Channel, Origin string
}

func (p Payment) ID() string      { return p.Channel }
func (p Payment) Account() string { return "LOCAL-DEMO-" + p.Channel }
func (p Payment) Create(_ context.Context, o payments.Order, _ string) (string, error) {
	p.State.Lock()
	defer p.State.Unlock()
	if p.State.Payments[o.ID] == "" {
		p.State.Payments[o.ID] = "pending"
	}
	return p.Origin + "/local-demo", p.State.save()
}
func (p Payment) Query(_ context.Context, o payments.Order) (payments.Result, error) {
	p.State.Lock()
	defer p.State.Unlock()
	state := p.State.Payments[o.ID]
	if state == "" {
		state = "not_found"
	}
	trade := ""
	if state == "paid" {
		trade = "LOCAL-DEMO-" + o.ID
	}
	return payments.Result{OrderID: o.ID, Status: state, TradeNo: trade, Amount: o.Amount}, nil
}
func (p Payment) Close(_ context.Context, o payments.Order) (bool, error) {
	p.State.Lock()
	defer p.State.Unlock()
	if p.State.Payments[o.ID] == "paid" {
		return false, nil
	}
	p.State.Payments[o.ID] = "closed"
	return true, p.State.save()
}
func (p Payment) Refund(_ context.Context, o payments.Order, r payments.Refund) (payments.Refunded, error) {
	p.State.Lock()
	defer p.State.Unlock()
	if r.ID == "" || r.Amount <= 0 || r.Amount > o.Amount || o.TradeNo != "LOCAL-DEMO-"+o.ID {
		return payments.Refunded{}, errors.New("invalid local payment refund")
	}
	key := "payment-refund/" + r.ID
	receipt := o.TradeNo + "/" + strconv.FormatInt(r.Amount, 10)
	if previous := p.State.Receipts[key]; previous != "" && previous != receipt {
		return payments.Refunded{}, errors.New("conflicting local refund receipt")
	}
	p.State.Receipts[key] = receipt
	return payments.Refunded{Confirmed: true, TradeNo: o.TradeNo}, p.State.save()
}
func (p Payment) QueryRefund(c context.Context, o payments.Order, r payments.Refund) (payments.Refunded, error) {
	p.State.Lock()
	defer p.State.Unlock()
	return payments.Refunded{Confirmed: p.State.Receipts["payment-refund/"+r.ID] == o.TradeNo+"/"+strconv.FormatInt(r.Amount, 10), TradeNo: o.TradeNo}, nil
}
func (p Payment) Notification(*http.Request) (payments.Result, error) {
	return payments.Result{}, errors.New("local demo does not accept payment webhooks")
}
