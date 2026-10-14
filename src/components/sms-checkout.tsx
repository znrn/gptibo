import { AsyncButton, PaymentConfirmed, RequestNotice } from './payment-feedback'
import { useState } from 'react'
import { ArrowRight, Check, LoaderCircle, Phone, RotateCcw, Search, TriangleAlert, X } from 'lucide-react'
import { CopyButton } from './copy-button'
import { Button } from '@/components/ui/button'
import { SuccessMark } from './brand'
import type { CheckoutProps } from './checkout'
import { orderLabel, type OrderAction } from '@/lib/demo-orders'

export function SmsCheckout({ order, onOpenChange, onAction, onOrders, busy, pendingAction, error }: CheckoutProps) {
  const [outcome, setOutcome] = useState('success')
  const [optionsOpen, setOptionsOpen] = useState(false)
  if (!order) return null
  if (order.redemptionBlocked) return <div className="order-service-paused">本订单已由客服处理，无法再次兑换。<br />如有疑问，请联系支持。</div>
  const code = order.sms?.code ?? (order.mode === 'demo' ? '123456' : '')
  const action = (value: OrderAction) => { if (busy) return; onAction(order.id, value) }
  return <div className="sms-checkout">
    {order.smsPhone && !['pending', 'cancelled'].includes(order.status) && <div className="sms-phone-panel"><div className="sms-phone-caption"><span><Phone size={14} />本次手机号</span><span>测试号码</span></div><div className="sms-phone-value"><strong>{order.smsPhone}</strong><CopyButton iconOnly variant="ghost" value={order.smsPhone!} label="复制测试手机号" /></div><p>测试用号码，不能接收真实短信。</p></div>}
    {error && <p role="alert" className="copy-error">{error}</p>}
    {order.refundHold || (order.refundedCents ?? 0) >= Math.round(order.price * 100) ? <div className="order-service-paused">{order.refundHold ? '售后申请正在处理中，服务暂时暂停。' : '本订单已全额退款。'}<br />可以在下方查看售后进度。</div> : <fieldset disabled={busy} aria-busy={busy} className="checkout-body" style={{ minWidth: 0 }}>
      {order.status === 'pending' && <>
        <div className="payment-method"><div className="alipay-mark">支</div><div><strong>支付宝</strong><p>付款流程联调</p></div><Check size={17} /></div>
        <p className="sms-description">付款步骤通过后获取手机号，再查询接码结果。</p>
        <div className="total-row"><span>本次合计</span><strong>¥{order.price.toFixed(2)}</strong></div><AsyncButton className="primary-action" loading={pendingAction === 'pay'} pendingLabel="正在校验付款步骤" onClick={() => action('pay')}>跳过付款，继续测试 <ArrowRight /></AsyncButton><button className="text-button quiet centered" onClick={() => action('cancel')}>{pendingAction === 'cancel' ? '正在关闭订单…' : '暂不购买，关闭此订单'}</button>
      </>}
      {order.status === 'session' && !order.smsPhone && <>
        <PaymentConfirmed>付款步骤已通过，无需提交 Session。</PaymentConfirmed>
        <div className="result-body"><div className="result-icon"><Phone size={30} strokeWidth={1.5} /></div><h3>先获取本次手机号</h3><p>获取后可复制手机号，再查询接码结果。<br />当前分配测试用号码。</p></div>
        <AsyncButton className="primary-action" loading={pendingAction === 'acquire-phone'} pendingLabel="正在获取测试手机号" onClick={() => action('acquire-phone')}>获取测试手机号 <ArrowRight /></AsyncButton>
      </>}
      {order.status === 'session' && order.smsPhone && <>
        <p className="sms-query-description">手机号已保留在这笔订单中。点击「查询结果」查看短信状态与验证码。</p>
        <details className="sms-demo-options" open={optionsOpen} onToggle={event => setOptionsOpen(event.currentTarget.open)}><summary>测试场景</summary><label htmlFor="sms-outcome">接收结果</label><select id="sms-outcome" value={outcome} onChange={event => setOutcome(event.target.value)}><option value="success">收到验证码</option><option value="failed">接码超时</option></select></details>
        <AsyncButton className="primary-action" loading={pendingAction?.startsWith('submit-')} pendingLabel="正在发起查询" onClick={() => action(outcome === 'failed' ? 'submit-failed' : 'submit-success')}><Search size={16} />查询结果 <ArrowRight /></AsyncButton>
      </>}
      {order.status === 'processing' && <div className="result-body" role="status"><div className="result-icon"><LoaderCircle className="processing-spinner" size={34} strokeWidth={1.5} /></div><h3>正在查询短信结果</h3><p>你可以关闭窗口，稍后从订单查看。<br />测试结果约 5 秒返回。</p><div className="processing-track"><span /></div><Button variant="outline" className="primary-action sms-result-action" onClick={onOrders}>查看我的订单</Button></div>}
      {order.status === 'success' && <div className="result-body"><SuccessMark /><h3>测试验证码</h3><p>测试用结果，不能用于真实验证。</p><div className="sms-code" aria-label={'测试验证码 ' + code}>{code}</div><CopyButton variant="outline" className="primary-action" value={code} label="复制测试验证码" /><Button className="primary-action sms-result-action" onClick={onOrders}>查看我的订单 <ArrowRight /></Button></div>}
      {order.status === 'failed' && <div className="result-body"><div className="result-icon warning"><TriangleAlert size={30} /></div><h3>暂未收到验证码</h3><p>当前测试场景为短信等待超时。<br />可使用本次测试号码重新查询，无需重复付款。</p><AsyncButton className="primary-action" loading={pendingAction === 'retry'} pendingLabel="正在恢复查询" onClick={() => action('retry')}><RotateCcw size={16} />返回查询</AsyncButton></div>}
      {order.status === 'cancelled' && <div className="result-body"><div className="result-icon"><X size={30} /></div><h3>订单已关闭</h3><p>没有产生实际扣款。</p><Button variant="outline" className="primary-action" onClick={() => onOpenChange(false)}>返回商品详情</Button></div>}
    </fieldset>}
      <div className="checkout-action-feedback">{busy && <RequestNotice key={pendingAction} label={pendingAction === 'pay' ? '正在校验付款步骤，请勿重复提交。' : pendingAction === 'acquire-phone' ? '正在获取本次测试号码，完成后自动显示。' : '正在保存操作，完成后自动继续。'} />}</div>
      <span className="sr-only" role="status">{orderLabel(order)}</span>
    </div>
}
