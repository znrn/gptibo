import {test} from 'node:test'
import assert from 'node:assert/strict'
import {DatabaseSync} from 'node:sqlite'
import {mkdtempSync,readFileSync,readdirSync,rmSync} from 'node:fs'
import {tmpdir} from 'node:os'
import {join} from 'node:path'
import {fileURLToPath} from 'node:url'
import {exportSnapshot} from './export-d1.mjs'

test('legacy export accepts empty Worker ticket tables and refuses notification history',()=>{
  const dir=mkdtempSync(join(tmpdir(),'gptibo-export-'))
  const migrations=fileURLToPath(new URL('../../../migrations/',import.meta.url))
  const source=join(dir,'source.sqlite'),db=new DatabaseSync(source)
  try {
    for(const file of readdirSync(migrations).filter(f=>f.endsWith('.sql')).sort())db.exec(readFileSync(join(migrations,file),'utf8'))
    const exported=exportSnapshot(source,join(dir,'empty.json'))
    assert.ok(exported.tables>30)
    db.exec('UPDATE partner_ticket_notifications SET version=1 WHERE id=1')
    assert.throws(()=>exportSnapshot(source,join(dir,'not-empty.json')),/refuses to omit ticket data/)
  }finally{db.close();rmSync(dir,{recursive:true,force:true})}
})
