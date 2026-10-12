import assert from 'node:assert/strict';
import test, { afterEach } from 'node:test';
import { checkout, createServer } from '../src/server.js';

const originalFetch = globalThis.fetch;
afterEach(() => { globalThis.fetch = originalFetch; });
function response(body, status = 200) { return { ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body) }; }
function ready(channel) { return { checkout_id: 'checkout-1', state: 'awaiting_payment', order: { id: 'ord-1', status: 'pending_payment' }, inventory: { reservation_id: 'rsv-1', status: 'reserved' }, payment: { id: 'pay-1', status: 'pending', checkout_url: 'http://pay.test', client_token: 'token-1' } }; }
function invoke(handler, request) {
 return new Promise((resolve, reject) => {
  const output = { status: 200, headers: {} };
  Promise.resolve(handler(request, { writeHead(status,headers) {output.status=status;output.headers=headers}, end(body) {output.body=body?JSON.parse(body):null;resolve(output)} })).catch(reject);
 });
}
for (const variant of ['desktop','app']) {
 test(`${variant} checkout keeps its contract through the durable coordinator`, async () => {
  globalThis.fetch = async (url,options) => {
   assert.equal(url,'http://order/v1/checkouts');
   const input=JSON.parse(options.body);
   assert.equal(input.channel,variant==='app'?'app':'web');
   assert.equal(options.headers['idempotency-key'],'checkout-key');
   return response(ready(),201);
  };
  const result=await checkout({user_id:'user-1',items:[{sku:'mug',quantity:1}]},variant,{order:'http://order'},{'idempotency-key':'checkout-key'});
  assert.equal(result.payment.id,'pay-1');
  if(variant==='desktop') { assert.equal(result.order.total.cents,650);assert.equal(result.payment.redirectUrl,'http://pay.test');assert.equal(result.inventory.reservationId,'rsv-1'); }
  else {assert.equal(result.amount.cents,650);assert.equal(result.payment.clientToken,'token-1');assert.equal(result.reservationId,'rsv-1');}
 });
}
test('audit API failure is isolated from checkout',async()=>{
 globalThis.fetch=async url=>url.includes('/v1/audit/events')?response({error:'unavailable'},503):response(ready(),201);
 const result=await invoke(createServer('desktop',{audit:'http://audit'}),{method:'GET',url:'/api/audit/events',headers:{}});
 assert.equal(result.status,503);
 assert.equal((await checkout({items:[{sku:'mug',quantity:1}]},'desktop')).order.id,'ord-1');
});
test('audit filters are forwarded to the centralized store',async()=>{
 globalThis.fetch=async url=>{assert.equal(url,'http://audit/v1/audit/events?service=payment&limit=20');return response({events:[{event_id:'e'}],count:1})};
 const result=await invoke(createServer('desktop',{audit:'http://audit'}),{method:'GET',url:'/api/audit/events?service=payment&limit=20',headers:{}});
 assert.equal(result.body.source,'audit-worker');assert.equal(result.body.count,1);
});
test('server catalog controls prices regardless of client-supplied amounts',async()=>{
 const amounts=[];
 globalThis.fetch=async(_,options)=>{amounts.push(JSON.parse(options.body).total_cents);return response(ready(),201)};
 for(const price of [0,-1,1,999999]) {assert.equal((await checkout({items:[{sku:'mug',quantity:2,unit_price_cents:price}]},'desktop')).order.total.cents,1300)}
 assert.deepEqual(amounts,[1300,1300,1300,1300]);
});
test('invalid SKUs, quantities and overflowing totals never create an order',async()=>{
 globalThis.fetch=async()=>assert.fail('invalid input reached upstream');
 for(const item of [null,{sku:'missing',quantity:1},{sku:'mug',quantity:'1'},{sku:'mug',quantity:0},{sku:'mug',quantity:1.5},{sku:'mug',quantity:Number.MAX_SAFE_INTEGER}]) await assert.rejects(checkout({items:[item]},'desktop'),error=>error.status===400);
});
test('processing checkouts expose a durable status resource',async()=>{
 globalThis.fetch=async()=>response({checkout_id:'c1',state:'processing'},202);
 const result=await invoke(createServer('desktop',{order:'http://order'}),{method:'GET',url:'/api/checkouts/c1',headers:{}});
 assert.equal(result.status,202);assert.equal(result.body.checkout_id,'c1');
});
