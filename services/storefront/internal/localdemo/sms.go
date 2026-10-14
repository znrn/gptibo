package localdemo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"gptibo/storefront/internal/orders"
	"gptibo/storefront/internal/store"
)

// SMS uses reserved fictional numbers and a fixed, explicitly simulated code.
// State stays on the actual order so payment, ownership, version and refund
// guards apply, and pending delivery survives application restarts.
type SMS struct{ Store *store.Store }

func (SMS) Enabled(plan string) bool { return plan == "codex-sms-once" }
func (s SMS) Snapshot(_ context.Context, _ pgx.Tx, o orders.Order, _ orders.CreateInput) error {
	if !s.Enabled(o.Plan) {
		return orders.Conflict()
	}
	return nil
}
func (s SMS) Action(o orders.Order, in orders.ActionInput) (orders.Order, error) {
	next := o
	if !s.Enabled(o.Plan) || o.Mode != "live" || o.Test != 0 || o.Payment != "paid" || o.Hold != 0 || o.Refunded != 0 || o.Blocked != 0 {
		return o, orders.Conflict()
	}
	switch in.Action {
	case "acquire-phone":
		if o.Status != "session" || o.Phone != nil {
			return o, orders.Conflict()
		}
		phone := "+1 202-555-0147"
		next.Phone = &phone
	case "submit-success", "submit-failed":
		if o.Status != "session" || o.Phone == nil {
			return o, orders.Conflict()
		}
		now := time.Now().UnixMilli()
		outcome := "success"
		if in.Action == "submit-failed" {
			outcome = "failed"
		}
		next.Status, next.Started, next.Outcome = "processing", &now, &outcome
	case "retry":
		if o.Status != "failed" {
			return o, orders.Conflict()
		}
		next.Status, next.Started, next.Outcome = "session", nil, nil
	default:
		return o, orders.Conflict()
	}
	return next, nil
}
func (SMS) View(o orders.Order) map[string]any {
	var code any
	if o.Status == "success" && o.Payment == "paid" && o.Hold == 0 && o.Refunded == 0 && o.Blocked == 0 {
		code = "123456"
	}
	return map[string]any{"simulated": true, "code": code}
}
func (s SMS) Reconcile(ctx context.Context) error {
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	rows, e := orders.Rows(ctx, tx, `SELECT id FROM orders WHERE plan='codex-sms-once' AND mode='live' AND is_test=0 AND status='processing' AND payment_status='paid' AND refund_hold=0 AND refunded_cents=0 AND redemption_blocked=0 AND started_at<=$1 ORDER BY started_at LIMIT 20 FOR UPDATE SKIP LOCKED`, time.Now().UnixMilli()-5000)
	if e != nil {
		return e
	}
	for _, r := range rows {
		o, e := orders.Get(ctx, tx, r["id"].(string), nil, false)
		if e != nil {
			return e
		}
		if o.Outcome == nil || (*o.Outcome != "success" && *o.Outcome != "failed") {
			continue
		}
		next := o
		next.Status = *o.Outcome
		if _, e = orders.Change(ctx, tx, o, next, "system", "local-sms-result", "本机模拟短信结果："+next.Status); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
