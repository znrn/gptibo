import { ApiError, body, json, type Env } from './platform.ts'
import { type Order } from './order-state.ts'
import { findPayment, refundThroughProvider } from './payments/service.ts'
import { findRechargeOrder, platformOrderStatus, reconcilePlatformRefund, refundThroughRecharge, RechargeRefundRejected } from './recharge/service.ts'

type Case = { id: string; order_id: string; status: string; requested_cents: number; approved_cents: number | null; version: number; reason: string; note: string; created_at: number; updated_at: number }
const text = (value: unknown, label: string, max = 500) => {
  if (typeof value !== 'string' || !value.trim() || value.trim().length > max) throw new ApiError(`请填写有效的${label}（最多 ${max} 字）。`)
  return value.trim()
}
const publicCase = (c: Case) => ({ id: c.id, status: c.status, requested_cents: c.requested_cents, approved_cents: c.approved_cents, version: c.version, reason: c.reason, note: c.note, created_at: c.created_at, updated_at: c.updated_at })
const event = (env: Env, id: string, caseId: string, actor: string, note: string) => env.INTEREST_DB.prepare('INSERT INTO refund_events(id,case_id,status,note,actor,created_at) SELECT ?,id,status,?,?,? FROM refund_cases WHERE id=? AND last_event_id=?').bind(id, note, actor, Date.now(), caseId, id)
async function checkWritten(env: Env, id: string) {
  if (!await env.INTEREST_DB.prepare('SELECT id FROM refund_events WHERE id=?').bind(id).first()) throw new ApiError('订单、售后或提现状态已更新，请刷新后重试。', 409)
}
export async function customerRefunds(request: Request, env: Env, order: Order) {
  const db = env.INTEREST_DB
  if (request.method === 'GET') {
    const cases = (await db.prepare('SELECT * FROM refund_cases WHERE order_id=? ORDER BY created_at DESC').bind(order.id).all<Case>()).results
    const events = (await db.prepare('SELECT e.case_id,e.status,e.note,e.actor,e.created_at FROM refund_events e JOIN refund_cases r ON r.id=e.case_id WHERE r.order_id=? ORDER BY e.created_at,e.id').bind(order.id).all()).results
    return json({ cases: cases.map(publicCase), events, orderVersion: order.version, refundableCents: order.price_cents - order.refunded_cents })
  }
  const data = await body(request); const now = Date.now(); const eventId = crypto.randomUUID()
  if (order.mode === 'demo' && env.ORDER_TESTING_ENABLED !== 'true') throw new ApiError('此历史记录仅支持查看售后进度。', 409)
  if (data.action === 'cancel') {
    const row = await db.prepare('SELECT * FROM refund_cases WHERE id=? AND order_id=?').bind(String(data.id), order.id).first<Case>()
    if (!row || row.status !== 'pending' || row.version !== data.version) throw new ApiError('该申请已处理，不能撤回。', 409)
    await db.batch([
      db.prepare("UPDATE refund_cases SET status='cancelled',version=version+1,updated_at=?,last_event_id=?,note='客户撤回申请' WHERE id=? AND status='pending' AND version=?").bind(now, eventId, row.id, row.version),
      db.prepare('UPDATE orders SET refund_hold=0,version=version+1,updated_at=? WHERE id=? AND EXISTS(SELECT 1 FROM refund_cases WHERE id=? AND last_event_id=?)').bind(now, order.id, row.id, eventId),
      event(env, eventId, row.id, 'customer', '客户撤回申请'),
    ])
    await checkWritten(env, eventId); return json({ ok: true })
  }
  const reason = text(data.reason, '售后原因')
  if (typeof data.key !== 'string' || !/^[a-f0-9-]{36}$/.test(data.key)) throw new ApiError('申请编号无效。')
  const existing = await db.prepare('SELECT id FROM refund_cases WHERE order_id=? AND request_key=?').bind(order.id, data.key).first()
  if (existing) return json({ ok: true })
  if (order.mode === 'live' && (await findRechargeOrder(env, order.id))?.financial_hold) throw new ApiError('平台退款金额正在核对，请联系客服后再申请。', 409)
  if (order.payment_status === 'unpaid' || order.status === 'cancelled' || order.refunded_cents >= order.price_cents) throw new ApiError('这笔订单没有可申请的已付金额。', 409)
  if (data.version !== order.version || order.refund_hold) throw new ApiError('已有售后申请或订单已更新，请刷新查看。', 409)
  if (!(await env.INTEREST_LIMITER.limit({ key: 'refund:' + order.owner_hash })).success) throw new ApiError('申请过于频繁，请稍后重试。', 429)
  const id = crypto.randomUUID()
  await db.batch([
    db.prepare("INSERT INTO refund_cases(id,order_id,request_key,reason,requested_cents,created_at,updated_at,last_event_id) SELECT ?,id,?,?,price_cents-refunded_cents,?,?,? FROM orders WHERE id=? AND version=? AND refund_hold=0 AND payment_status<>'unpaid' AND refunded_cents<price_cents AND NOT EXISTS(SELECT 1 FROM email_deliveries d WHERE d.order_id=orders.id AND d.lease_until>?) AND NOT EXISTS(SELECT 1 FROM account_supplier_orders k WHERE k.order_id=orders.id AND (k.lease_until>? OR k.state IN ('creating','unknown'))) AND NOT EXISTS(SELECT 1 FROM delivery_supplier_orders n WHERE n.order_id=orders.id AND (n.lease_until>? OR n.state IN ('creating','unknown','pending'))) AND NOT EXISTS(SELECT 1 FROM xgift_purchases x WHERE x.order_id=orders.id AND (x.lease_until>? OR x.state IN ('creating','unknown','pending'))) AND NOT EXISTS(SELECT 1 FROM recharge_orders t WHERE t.order_id=orders.id AND t.financial_hold=1) ON CONFLICT DO NOTHING").bind(id, data.key, reason, now, now, eventId, order.id, order.version, now, now, now, now),
    db.prepare('UPDATE orders SET refund_hold=1,version=version+1,updated_at=? WHERE id=? AND EXISTS(SELECT 1 FROM refund_cases WHERE id=?)').bind(now, order.id, id),
    // Unreviewed requests can safely be returned. Reviewed transfers must be reconciled explicitly.
    db.prepare("UPDATE partner_payouts SET status='rejected',note='关联订单申请售后，收益已重新核算，请稍后重新申请',handled_at=?,version=version+1 WHERE id=(SELECT payout_id FROM orders WHERE id=?) AND status='pending' AND reviewed_at IS NULL AND EXISTS(SELECT 1 FROM refund_cases WHERE id=?)").bind(now, order.id, id),
    db.prepare("UPDATE partner_debits SET payout_id=NULL WHERE payout_id IN (SELECT ps.id FROM partner_payouts ps JOIN orders o ON o.payout_id=ps.id WHERE o.id=? AND ps.status='rejected') AND EXISTS(SELECT 1 FROM refund_cases WHERE id=?)").bind(order.id, id),
    db.prepare("UPDATE orders SET payout_id=NULL WHERE payout_id IN (SELECT ps.id FROM partner_payouts ps JOIN orders o ON o.payout_id=ps.id WHERE o.id=? AND ps.status='rejected') AND EXISTS(SELECT 1 FROM refund_cases WHERE id=?)").bind(order.id, id),
    event(env, eventId, id, 'customer', reason),
  ])
  await checkWritten(env, eventId); return json({ ok: true }, 201)
}

export async function adminRefunds(request: Request, env: Env, url: URL) {
  if (url.pathname === '/api/admin/refunds/notifications') {
    if (request.method !== 'GET') throw new ApiError('请求方式不支持。', 405)
    // Retained submissions form a stable version: status changes and imported
    // supplier refunds must not masquerade as new customer requests.
    return json(await env.INTEREST_DB.prepare("SELECT COUNT(CASE WHEN request_key NOT LIKE 'external-refund:%' THEN 1 END) version,COUNT(CASE WHEN status IN ('pending','approved') THEN 1 END) pending FROM refund_cases").first())
  }
  const db = env.INTEREST_DB; const path = url.pathname
  if (path === '/api/admin/refunds' && request.method === 'GET') {
    const page = Number(url.searchParams.get('page') ?? 1); const status = url.searchParams.get('status') ?? ''
    if (!Number.isInteger(page) || page < 1 || page > 10000 || (status && !['pending', 'approved', 'refunded', 'rejected', 'cancelled'].includes(status))) throw new ApiError('筛选条件无效。')
    const rows = (await db.prepare("SELECT r.*,o.plan_name,o.mode,o.customer_email,o.partner_name FROM refund_cases r JOIN orders o ON o.id=r.order_id WHERE (?='' OR r.status=?) ORDER BY CASE WHEN r.status IN ('pending','approved') THEN 0 ELSE 1 END,r.created_at DESC,r.id DESC LIMIT 20 OFFSET ?").bind(status, status, (page - 1) * 20).all()).results
    const count = await db.prepare("SELECT COUNT(*) total FROM refund_cases WHERE (?='' OR status=?)").bind(status, status).first<{ total: number }>()
    return json({ rows, total: count?.total ?? 0 })
  }
  const match = path.match(/^\/api\/admin\/refunds\/([a-f0-9-]{36})$/)
  if (!match) throw new ApiError('售后申请不存在。', 404)
  const row = await db.prepare('SELECT * FROM refund_cases WHERE id=?').bind(match[1]).first<Case>()
  if (!row) throw new ApiError('售后申请不存在。', 404)
  const order = await db.prepare('SELECT * FROM orders WHERE id=?').bind(row.order_id).first<Order & { payout_id: string | null }>()
  if (!order) throw new ApiError('订单不存在。', 404)
  const payout = order.payout_id ? await db.prepare('SELECT id,status,reviewed_at FROM partner_payouts WHERE id=?').bind(order.payout_id).first<{ id: string; status: string; reviewed_at: number | null }>() : null
  if (request.method === 'GET') {
    const events = (await db.prepare('SELECT * FROM refund_events WHERE case_id=? ORDER BY created_at,id').bind(row.id).all()).results
    const partnerDebit = await db.prepare('SELECT d.amount_cents,d.payout_id,p.status payout_status FROM partner_debits d LEFT JOIN partner_payouts p ON p.id=d.payout_id WHERE d.refund_id=?').bind(row.id).first()
    const channel = order.mode === 'live' ? (await findRechargeOrder(env, order.id) ? 'recharge' : (await findPayment(env, order.id))?.provider ?? null) : null
    return json({ refund: row, hasPartner: order.partner_id !== null, partnerDebit, paymentProvider: channel, platformStatus: channel === 'recharge' ? await platformOrderStatus(env, order) : null, order: { id: order.id, plan: order.plan_name, plan_id: order.plan, price_cents: order.price_cents, refunded_cents: order.refunded_cents, customer_email: order.customer_email, mode: order.mode, status: order.status, partner_name: order.partner_name }, payout, events })
  }
  const data = await body(request); const note = text(data.note, '处理说明'); const now = Date.now(); const eventId = crypto.randomUUID()
  if (row.version !== data.version) throw new ApiError('售后已更新，请刷新。', 409)
  if (data.action === 'approve' && row.status === 'pending') {
    const amount = Number(data.amountCents)
    if (!Number.isSafeInteger(amount) || amount < 1 || amount > row.requested_cents || amount > order.price_cents - order.refunded_cents) throw new ApiError('退款金额超出可退范围。')
    if (order.status !== 'success' && amount !== order.price_cents - order.refunded_cents) throw new ApiError('未交付或交付失败的订单请按剩余已付金额全额退款。')
    await db.batch([
      db.prepare("UPDATE refund_cases SET status='approved',approved_cents=?,note=?,version=version+1,updated_at=?,last_event_id=? WHERE id=? AND status='pending' AND version=? AND EXISTS(SELECT 1 FROM orders WHERE id=order_id AND refund_hold=1 AND version=?)").bind(amount, note, now, eventId, row.id, row.version, order.version),
      event(env, eventId, row.id, 'admin', note),
    ])
  } else if (data.action === 'reject' && row.status === 'pending') {
    await db.batch([
      db.prepare("UPDATE refund_cases SET status='rejected',note=?,version=version+1,updated_at=?,last_event_id=? WHERE id=? AND status='pending' AND version=?").bind(note, now, eventId, row.id, row.version),
      db.prepare('UPDATE orders SET refund_hold=0,updated_at=?,version=version+1 WHERE id=? AND EXISTS(SELECT 1 FROM refund_cases WHERE id=? AND last_event_id=?)').bind(now, order.id, row.id, eventId),
      event(env, eventId, row.id, 'admin', note),
    ])
  } else if (data.action === 'reconcile-platform-refund' && row.status === 'approved') {
    await executeRefund(env, row, order, note, 'admin', undefined, true)
    return json({ ok: true })
  } else if ((data.action === 'complete' || data.action === 'execute-refund') && row.status === 'approved') {
    const channel = order.mode === 'live' ? await findRechargeOrder(env, order.id) ?? await findPayment(env, order.id) : null
    if ((channel && data.action !== 'execute-refund') || (!channel && data.action !== 'complete')) throw new ApiError('请刷新详情并使用该订单对应的退款方式。', 409)
    await executeRefund(env, row, order, note, 'admin', data.reference)
    return json({ ok: true })
  } else throw new ApiError('当前状态不支持此操作。', 409)
  await checkWritten(env, eventId)
  return json({ ok: true })
}

async function executeRefund(env: Env, row: Case, order: Order & { payout_id: string | null }, note: string, actor: string, manualReference?: unknown, reconcilePlatform = false) {
  const db = env.INTEREST_DB; const now = Date.now(); const eventId = crypto.randomUUID()
  const payout = order.payout_id ? await db.prepare('SELECT status FROM partner_payouts WHERE id=?').bind(order.payout_id).first<{ status: string }>() : null
  if (payout?.status === 'pending') throw new ApiError('关联提现已进入审核转账流程，请先核对并完成或退回该提现，再登记退款。', 409)
  let reference: string
  try { reference = reconcilePlatform ? await reconcilePlatformRefund(env, order, row.approved_cents!) : order.mode === 'demo' ? 'DEMO-' + row.id : await refundThroughRecharge(env, order, row.id, row.approved_cents!) ?? await refundThroughProvider(env, order, row.id, row.approved_cents!) ?? text(manualReference, '实际退款流水号', 100) }
  catch (e) {
    if (!(e instanceof RechargeRefundRejected)) throw e
    await db.batch([
      db.prepare("UPDATE refund_cases SET status='rejected',note=?,version=version+1,updated_at=?,last_event_id=? WHERE id=? AND status='approved' AND version=?").bind('平台已明确拒绝或取消本次退款：' + note, now, eventId, row.id, row.version),
      db.prepare('UPDATE orders SET refund_hold=0,updated_at=?,version=version+1 WHERE id=? AND EXISTS(SELECT 1 FROM refund_cases WHERE id=? AND last_event_id=?)').bind(now, order.id, row.id, eventId),
      event(env, eventId, row.id, actor, '平台已确认本次退款未执行，结束申请并解除本次售后冻结'),
    ])
    await checkWritten(env, eventId); return
  }
  const margin = order.cost_cents === null ? 0 : order.price_cents - order.cost_cents
  const afterRefund = order.refunded_cents + row.approved_cents!
  let debit = Math.floor(margin * afterRefund / order.price_cents) - Math.floor(margin * order.refunded_cents / order.price_cents)
  const supplier = order.mode === 'live' ? await findRechargeOrder(env, order.id) : null
  const forfeit = order.plan === 'duolingo-12m'
  if ((supplier || forfeit) && payout?.status === 'paid') {
    const paid = await db.prepare('SELECT profit_cents FROM partner_payout_items WHERE payout_id=? AND order_id=?').bind(order.payout_id, order.id).first<{ profit_cents: number }>()
    if (!paid || (supplier && supplier.merchant_margin_cents === null)) throw new ApiError('代理历史收益尚未核对，请联系管理员。', 409)
    const previous = await db.prepare('SELECT COALESCE(SUM(d.amount_cents),0) amount FROM partner_debits d JOIN refund_cases r ON r.id=d.refund_id WHERE r.order_id=?').bind(order.id).first<{ amount: number }>()
    const remaining = forfeit ? 0 : Math.max(0, Math.min(margin - Math.floor(margin * afterRefund / order.price_cents), supplier!.merchant_margin_cents!))
    debit = Math.max(0, paid.profit_cents - (previous?.amount ?? 0) - remaining)
  }
  await db.batch([
    db.prepare("UPDATE refund_cases SET status='refunded',reference=?,note=?,version=version+1,updated_at=?,last_event_id=? WHERE id=? AND status='approved' AND version=? AND EXISTS(SELECT 1 FROM orders o WHERE o.id=order_id AND o.version=? AND o.refund_hold=1 AND o.refunded_cents+approved_cents<=o.price_cents AND NOT EXISTS(SELECT 1 FROM partner_payouts p WHERE p.id=o.payout_id AND p.status='pending')) AND (?=0 OR EXISTS(SELECT 1 FROM recharge_orders t WHERE t.order_id=refund_cases.order_id AND t.refunded_cents=(SELECT refunded_cents FROM orders WHERE id=refund_cases.order_id)+refund_cases.approved_cents AND t.payment_status IN ('partially_refunded','refunded')))").bind(reference, note, now, eventId, row.id, row.version, order.version, Number(reconcilePlatform)),
    db.prepare('UPDATE orders SET refunded_cents=refunded_cents+?,refund_hold=0,version=version+1,updated_at=? WHERE id=? AND EXISTS(SELECT 1 FROM refund_cases WHERE id=? AND last_event_id=?)').bind(row.approved_cents!, now, order.id, row.id, eventId),
    db.prepare('UPDATE recharge_orders SET financial_hold=CASE WHEN refunded_cents=(SELECT refunded_cents FROM orders WHERE id=?) THEN 0 ELSE 1 END WHERE order_id=? AND EXISTS(SELECT 1 FROM refund_cases WHERE id=? AND last_event_id=?)').bind(order.id, order.id, row.id, eventId),
    db.prepare("INSERT INTO partner_debits(id,partner_id,refund_id,amount_cents,created_at) SELECT ?,o.partner_id,?,?,? FROM orders o JOIN partner_payouts p ON p.id=o.payout_id WHERE o.id=? AND o.mode='live' AND p.status='paid' AND ?>0 AND EXISTS(SELECT 1 FROM refund_cases WHERE id=? AND last_event_id=?)").bind(crypto.randomUUID(), row.id, debit, now, order.id, debit, row.id, eventId),
    event(env, eventId, row.id, actor, (reconcilePlatform ? '已核对平台原订单退款并完成记账：' : order.mode === 'demo' ? '历史售后记录已结束（无资金往来）：' : '已登记实际退款：') + note),
  ])
  await checkWritten(env, eventId)
}

// Poll only refunds already initiated by an administrator; never issue unreviewed refunds.
export async function reconcileRechargeRefunds(env: Env) {
  const rows = (await env.INTEREST_DB.prepare("SELECT r.* FROM refund_cases r JOIN recharge_refunds t ON t.case_id=r.id WHERE r.status='approved' ORDER BY t.updated_at LIMIT 10").all<Case>()).results
  let completed = 0; let pending = 0
  for (const row of rows) {
    try {
      const order = await env.INTEREST_DB.prepare('SELECT * FROM orders WHERE id=?').bind(row.order_id).first<Order & { payout_id: string | null }>()
      if (!order) throw new Error()
      await executeRefund(env, row, order, '自动核对已发起的退款结果', 'system'); completed++
    } catch { pending++; await env.INTEREST_DB.prepare('UPDATE recharge_refunds SET updated_at=? WHERE case_id=?').bind(Date.now(), row.id).run() }
  }
  return { checked: rows.length, completed, pending }
}
