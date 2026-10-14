import assert from 'node:assert/strict';
import test from 'node:test';
import { pollCheckout } from '../lib/checkout-flow.mjs';
const response=(state,status=202)=>({ok:status<400,status,json:async()=>typeof state==='string'?{checkoutId:'c1',state}:state});

test('settling and refund_pending do not finish polling or report payment success',async()=>{
 const updates=[],next=[response('settling'),response('refund_pending'),response({error:'reservation_expired'},409)];
 await assert.rejects(pollCheckout(response('processing'),{wait:async()=>{},fetchStatus:async id=>{assert.equal(id,'c1');return next.shift();},onUpdate:body=>updates.push(body.state)}),error=>error.message==='reservation_expired' && error.terminal===true);
 assert.deepEqual(updates,['processing','settling','refund_pending']);assert.equal(next.length,0);
});

for(const state of ['awaiting_payment','paid']) test(`${state} is the only successful polling outcome`,async()=>{
 const result=await pollCheckout(response('processing'),{wait:async()=>{},fetchStatus:async()=>response(state,200)});
 assert.equal(result.state,state);
 const immediate=await pollCheckout(response(state,201),{fetchStatus:()=>assert.fail('ready checkout was polled')});
 assert.equal(immediate.state,state);
});

test('polling exhaustion retains the workflow identity and is retryable',async()=>{
 let polls=0;
 await assert.rejects(pollCheckout(response('settling'),{maxPolls:3,wait:async()=>{},fetchStatus:async()=>{polls++;return response('settling');}}),error=>error.message==='checkout_pending' && error.terminal===false && error.checkoutId==='c1');
 assert.equal(polls,3);
});

for(const status of [409,500,503]) test(`HTTP ${status} has the correct checkout-attempt retention policy`,async()=>{
 await assert.rejects(pollCheckout(response({error:'upstream'},status),{fetchStatus:()=>assert.fail('error was polled')}),error=>error.terminal===(status<500));
});

test('network loss and missing status identity never become success',async()=>{
 await assert.rejects(pollCheckout(response('processing'),{wait:async()=>{},fetchStatus:async()=>{throw new Error('network');}}),error=>error.message==='network' && !error.terminal);
 await assert.rejects(pollCheckout(response({state:'settling'}),{fetchStatus:()=>assert.fail('undefined checkout id')}),error=>error.message==='checkout_status_missing' && error.terminal===false);
});
