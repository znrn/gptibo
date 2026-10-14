// Read a trusted, offline D1 SQLite snapshot. Never connects to Cloudflare.
import { DatabaseSync } from 'node:sqlite'
import { readFileSync, writeFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { supportedColumns } from './legacy-columns.mjs'

export const excluded = new Set(['admin_sessions','admin_email_login_codes','admin_login_limits','admin_totp_usage','partner_sessions','partner_login_codes','customer_email_sessions','customer_login_codes'])
export function exportSnapshot(input, output) {
  const specBytes = readFileSync(new URL('../internal/importer/legacy-schema.json', import.meta.url), 'utf8').replace(/\r\n/g, '\n')
  const spec = JSON.parse(specBytes)
  if (resolve(input) === resolve(output)) throw new Error('output must differ from source')
  const db = new DatabaseSync(input, { readOnly: true, enableLoadExtension: false })
  try {
    db.exec('BEGIN')
    let tables = db.prepare("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '_cf_%' AND name <> 'd1_migrations' ORDER BY name").all().map(r => r.name)
    // 0044 adds Worker tickets after this legacy import format was frozen.
    // Empty ticket tables need no conversion. Never silently drop actual tickets
    // or their notification history when exporting to the older import format.
    const tickets = ['partner_tickets', 'partner_ticket_events', 'partner_ticket_notifications']
    if (tickets.some(name => tables.includes(name))) {
      if (!tickets.every(name => tables.includes(name))) throw new Error('incomplete Worker ticket schema')
      if (db.prepare('SELECT count(*) n FROM partner_tickets').get().n ||
          db.prepare('SELECT count(*) n FROM partner_ticket_events').get().n ||
          db.prepare('SELECT count(*) n FROM partner_ticket_notifications WHERE id<>1 OR version<>0').get().n) {
        throw new Error('Worker tickets require a dedicated migration; legacy export refuses to omit ticket data')
      }
      tables = tables.filter(name => !tickets.includes(name))
    }
    if (JSON.stringify(tables) !== JSON.stringify(spec.tables.map(t => t.name))) throw new Error('source tables differ from supported D1 schema')
    const snapshot = { format: 1, schema: createHash('sha256').update(specBytes).digest('hex'), tables: {}, excluded: {} }
    for (const table of spec.tables) {
      const cols = db.prepare(`PRAGMA table_info("${table.name}")`).all().map(({name,type,pk}) => ({name,type,pk}))
      if (!supportedColumns(table, cols)) throw new Error('source schema differs: '+table.name)
      if (excluded.has(table.name)) { snapshot.excluded[table.name] = db.prepare(`SELECT count(*) n FROM "${table.name}"`).get().n; continue }
      const statement = db.prepare(`SELECT * FROM "${table.name}" ORDER BY ${table.name==='catalog_plans' ? 'rowid' : table.columns.filter(c=>c.pk).sort((a,b)=>a.pk-b.pk).map(c=>'"'+c.name+'"').join(',') || 'rowid'}`)
      statement.setReadBigInts(true)
      snapshot.tables[table.name] = statement.all().map(row => Object.fromEntries(Object.entries(row).map(([key,value]) => {
        if(value !== null && !['string','bigint','number'].includes(typeof value)) throw new Error('unsupported source value: '+table.name)
        if(typeof value === 'number' && !Number.isFinite(value)) throw new Error('nonfinite source value')
        return [key, value === null ? null : String(value)]
      })))
    }
    const data = JSON.stringify(snapshot)+'\n'
    writeFileSync(output, data, { flag: 'wx', mode: 0o600 })
    return { sha256: createHash('sha256').update(data).digest('hex'), tables: Object.keys(snapshot.tables).length, excluded: snapshot.excluded }
  } finally { db.close() }
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    if(process.argv.length!==4)throw new Error('usage: node export-d1.mjs <offline.sqlite> <snapshot.json>')
    console.log(JSON.stringify(exportSnapshot(process.argv[2],process.argv[3])))
  } catch (error) { console.error(error.message); process.exitCode=1 }
}
