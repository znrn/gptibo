import { isXGiftPlan } from '../../shared/xgift-products.ts'
import type { ServerOrder } from '@/lib/use-store'

// The API rechecks the original supplier result, version and leases before recovery.
export function canRecoverXGift(order: ServerOrder) {
  return isXGiftPlan(order.plan) && order.mode === 'live' && !order.isTest
    && order.paymentStatus === 'paid' && order.status === 'failed'
    && !order.refundHold && !order.redemptionBlocked && !order.refundedCents
    && order.xgift?.state === 'failed' && !order.xgift.receipt?.trim()
    && (order.xgift.failureCode === 'card_wallet_insufficient' || order.xgift.failureCode === 'x_price_changed' && order.xgift.recoveryAvailable === true)
}
