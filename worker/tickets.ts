import { ApiError, body, json, type Env, type Value } from './platform.ts'
import { requirePartner } from './partner-auth.ts'

const statuses = ['open', 'processing', 'waiting', 'resolved']
const validKey = (value: unknown): value is string => typeof value === 'string' && /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(value)
const conflict = () => new ApiError('工单或账号已更新，请刷新后重试；请勿重复使用请求编号。', 409)
function text(value: unknown, max: number) {
  if (typeof value !== 'string' || !value.trim() || [...value.trim()].length > max) throw new ApiError('请填写有效的工单标题、订单号和回复内容。')
  return value.trim()
}
type Ticket = { id: string; partner_id: string; order_id: string; subject: string; description: string; version: number }
type Event = { ticket_id: string; actor_role: string; actor_id: string; body: string; status: string; version: number }

// Admin callers have passed requireAdmin in commerce; partners are scoped here.
export async function ticketRoutes(request: Request, env: Env, url: URL, admin = false) {
  const partner = admin ? null : await requirePartner(request, env)
  const owner = partner?.id ?? ''
  const actor = admin ? env.ADMIN_USERNAME! : owner
  const role = admin ? 'admin' : 'partner'
  const path = url.pathname.slice(`/api/${role}/tickets`.length)
  const db = env.INTEREST_DB
  if (admin && path === '/notifications') {
    if (request.method !== 'GET') throw new ApiError('请求方式不支持。', 405)
    const summary = await db.prepare("SELECT version,(SELECT COUNT(*) FROM partner_tickets WHERE status IN ('open','processing')) pending FROM partner_ticket_notifications WHERE id=1").first()
    if (!summary) throw new ApiError('工单提醒暂不可用。', 503)
    return json(summary)
  }
  if (path === '' && request.method === 'GET') {
    const page = Number(url.searchParams.get('page') ?? 1)
    const query = (url.searchParams.get('q') ?? '').trim()
    const status = url.searchParams.get('status') ?? ''
    if (!Number.isInteger(page) || page < 1 || page > 10000 || [...query].length > 320 || (status && !statuses.includes(status))) throw new ApiError('工单筛选条件无效。')
    const where = " FROM partner_tickets t JOIN partners p ON p.id=t.partner_id WHERE (?='' OR t.partner_id=?) AND (?='' OR t.status=?) AND (?='' OR instr(lower(t.id||' '||t.order_id||' '||t.subject||' '||p.email),lower(?))>0)"
    const values: Value[] = [owner, owner, status, status, query, query]
    // D1 batch uses one transaction for the count and page snapshot.
    const result = await db.batch([
      db.prepare('SELECT COUNT(*) total' + where).bind(...values),
      db.prepare('SELECT t.id,t.order_id,t.subject,t.status,t.version,t.created_at,t.updated_at,p.name partner_name,p.email partner_email' + where + ' ORDER BY t.updated_at DESC,t.id LIMIT 20 OFFSET ?').bind(...values, (page - 1) * 20),
    ]) as { results: Record<string, unknown>[] }[]
    return json({ rows: result[1].results, total: result[0].results[0].total })
  }
  if (path === '' && request.method === 'POST' && partner) {
    const data = await body(request, 32768)
    if (!validKey(data.key)) throw new ApiError('工单请求编号无效。')
    const order = text(data.orderId, 128), subject = text(data.subject, 120), description = text(data.body, 5000)
    const existing = await db.prepare('SELECT * FROM partner_tickets WHERE id=?').bind(data.key).first<Ticket>()
    if (existing) {
      if (existing.partner_id !== owner || existing.order_id !== order || existing.subject !== subject || existing.description !== description) throw conflict()
      return json({ id: data.key })
    }
    if (!await db.prepare('SELECT id FROM orders WHERE id=? AND partner_id=?').bind(order, owner).first()) throw new ApiError('未找到该订单，请填写自己商店的本站订单号。', 404)
    const now = Date.now()
    await db.batch([
      db.prepare("INSERT INTO partner_tickets(id,partner_id,order_id,subject,description,last_event_id,created_at,updated_at) SELECT ?,p.id,o.id,?,?,?,?,? FROM partners p JOIN orders o ON o.partner_id=p.id WHERE p.id=? AND o.id=? AND p.active=1 AND p.auth_version=? AND NOT EXISTS(SELECT 1 FROM partner_ticket_events WHERE id=?) ON CONFLICT DO NOTHING").bind(data.key, subject, description, data.key, now, now, owner, order, partner.auth_version, data.key),
      db.prepare("INSERT INTO partner_ticket_events(id,ticket_id,actor_role,actor_id,body,status,version,created_at) SELECT id,id,'partner',partner_id,description,'open',0,created_at FROM partner_tickets WHERE id=? AND partner_id=? AND order_id=? AND subject=? AND description=? AND version=0 ON CONFLICT DO NOTHING").bind(data.key, owner, order, subject, description),
    ])
    const saved = await db.prepare('SELECT * FROM partner_tickets WHERE id=?').bind(data.key).first<Ticket>()
    if (!saved || saved.partner_id !== owner || saved.order_id !== order || saved.subject !== subject || saved.description !== description) throw conflict()
    return json({ id: data.key })
  }
  const match = path.match(/^\/([a-f0-9-]{36})(\/replies)?$/)
  if (!match || !validKey(match[1])) throw new ApiError('工单不存在。', 404)
  const ticket = await db.prepare("SELECT t.*,p.name partner_name,p.email partner_email FROM partner_tickets t JOIN partners p ON p.id=t.partner_id WHERE t.id=? AND (?='' OR t.partner_id=?)").bind(match[1], owner, owner).first<Ticket>()
  if (!ticket) throw new ApiError('工单不存在。', 404)
  if (!match[2] && request.method === 'GET') {
    const events = (await db.prepare('SELECT id,actor_role,body,status,created_at FROM partner_ticket_events WHERE ticket_id=? ORDER BY version').bind(ticket.id).all()).results
    const { partner_id: _owner, description: _description, ...detail } = ticket
    return json({ ...detail, events })
  }
  if (match[2] && request.method === 'POST') {
    const data = await body(request, 32768)
    const message = text(data.body, 5000)
    if (!validKey(data.key) || !Number.isSafeInteger(data.version) || Number(data.version) < 0 || typeof data.status !== 'string' || !statuses.includes(data.status) || (!admin && data.status !== 'open')) throw new ApiError('工单回复状态、版本或请求编号无效。')
    const expected = Number(data.version) + 1
    const same = (event: Event) => event.ticket_id === ticket.id && event.actor_role === role && event.actor_id === actor && event.body === message && event.status === data.status && event.version === expected
    const existing = await db.prepare('SELECT * FROM partner_ticket_events WHERE id=?').bind(data.key).first<Event>()
    if (existing) {
      if (!same(existing)) throw conflict()
      return json({ ok: true })
    }
    const now = Date.now()
    await db.batch([
      // Recheck partner ownership and account revocation in the write transaction.
      db.prepare("UPDATE partner_tickets SET status=?,version=version+1,updated_at=?,last_event_id=? WHERE id=? AND version=? AND NOT EXISTS(SELECT 1 FROM partner_ticket_events WHERE id=?) AND (?='' OR (partner_id=? AND EXISTS(SELECT 1 FROM partners WHERE id=? AND active=1 AND auth_version=?)))").bind(data.status, now, data.key, ticket.id, Number(data.version), data.key, owner, owner, owner, partner?.auth_version ?? 0),
      db.prepare('INSERT INTO partner_ticket_events(id,ticket_id,actor_role,actor_id,body,status,version,created_at) SELECT ?,id,?,?,?,status,version,? FROM partner_tickets WHERE id=? AND version=? AND last_event_id=? ON CONFLICT DO NOTHING').bind(data.key, role, actor, message, now, ticket.id, expected, data.key),
    ])
    const saved = await db.prepare('SELECT * FROM partner_ticket_events WHERE id=?').bind(data.key).first<Event>()
    if (!saved || !same(saved)) throw conflict()
    return json({ ok: true })
  }
  throw new ApiError('请求方式不支持。', 405)
}
