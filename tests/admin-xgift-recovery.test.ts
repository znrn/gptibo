import test from 'node:test'
import assert from 'node:assert/strict'
import { canRecoverXGift } from '../src/admin/xgift-recovery.ts'
import type { ServerOrder } from '../src/lib/use-store.ts'

const eligible = {
  plan: 'x-premium-3m', mode: 'live', isTest: false, paymentStatus: 'paid',
  status: 'failed', refundHold: false, redemptionBlocked: false, refundedCents: 0,
  xgift: { state: 'failed', receipt: null, failureCode: 'card_wallet_insufficient' },
} as ServerOrder

test('admin exposes protected X recovery only for paid, unrefunded wallet failures', () => {
  assert.equal(canRecoverXGift(eligible), true)
  assert.equal(canRecoverXGift({ ...eligible, plan: 'x-premium-6m' }), true)
  for (const change of [
    { plan: 'plus' }, { mode: 'demo' }, { isTest: true }, { paymentStatus: 'unpaid' },
    { status: 'processing' }, { status: 'success' }, { refundHold: true },
    { redemptionBlocked: true }, { refundedCents: 1 }, { xgift: null },
  ]) assert.equal(Boolean(canRecoverXGift({ ...eligible, ...change } as ServerOrder)), false, JSON.stringify(change))
  for (const change of [
    { state: 'unknown' }, { state: 'pending' }, { state: 'succeeded' },
    { receipt: 'payment-evidence' }, { failureCode: 'payment_pending' },
    { failureCode: 'checkout_inactive' }, { failureCode: 'x_price_changed' },
  ]) assert.equal(Boolean(canRecoverXGift({ ...eligible, xgift: { ...eligible.xgift!, ...change } })), false, JSON.stringify(change))
})

// Go can explicitly advertise price-change recovery; older Worker APIs cannot.
test('admin exposes price-change recovery only when the backend advertises support', () => {
  const priceChange = { ...eligible, xgift: { ...eligible.xgift!, failureCode: 'x_price_changed', recoveryAvailable: true } }
  assert.equal(canRecoverXGift(priceChange), true)
  assert.equal(Boolean(canRecoverXGift({ ...priceChange, refundHold: true })), false)
  assert.equal(Boolean(canRecoverXGift({ ...priceChange, xgift: { ...priceChange.xgift, recoveryAvailable: false } })), false)
})
