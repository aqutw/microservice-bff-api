const ready = body => ['awaiting_payment', 'paid'].includes(body?.state);

async function readResult(response) {
 const body = await response.json();
 if (!response.ok) throw Object.assign(new Error(body.error || 'checkout_failed'), { terminal: response.status >= 400 && response.status < 500, checkoutId: body.checkoutId || body.checkout_id });
 return body;
}

export async function pollCheckout(initial, { fetchStatus, onUpdate = () => {}, wait = ms => new Promise(resolve => setTimeout(resolve, ms)), maxPolls = 90 }) {
 let body = await readResult(initial);
 onUpdate(body);
 const id = body.checkoutId || body.checkout_id;
 for (let attempt = 0; !ready(body) && attempt < maxPolls; attempt++) {
  if (!id) throw Object.assign(new Error('checkout_status_missing'), { terminal: false });
  await wait(1000);
  body = await readResult(await fetchStatus(id));
  onUpdate(body);
 }
 if (!ready(body)) throw Object.assign(new Error('checkout_pending'), { terminal: false, checkoutId: id });
 return body;
}
