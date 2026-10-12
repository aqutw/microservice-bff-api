'use client';
import SessionControls from './auth/session-controls';
import { useEffect, useMemo, useRef, useState } from 'react';

const BFF_URL = '';
const fallbackProducts = [
  { sku: 'coffee-beans', name: '山嵐咖啡豆', unitPriceCents: 420 },
  { sku: 'tea-leaves', name: '冷杉烏龍茶', unitPriceCents: 280 },
  { sku: 'mug', name: '霧黑陶杯', unitPriceCents: 650 }
];

export default function Home() {
  const [products, setProducts] = useState(fallbackProducts);
  const [cart, setCart] = useState({});
  const [result, setResult] = useState(null);
  const [loading, setLoading] = useState(false);
  const checkoutAttempt = useRef(null);
  useEffect(() => {
    try { const pending = JSON.parse(sessionStorage.getItem('pending-cart') || '{}'); if (pending && typeof pending === 'object' && !Array.isArray(pending)) setCart(pending); sessionStorage.removeItem('pending-cart'); } catch {}
    fetch(`${BFF_URL}/api/catalog`).then(r => r.ok ? r.json() : null).then(data => data?.products && setProducts(data.products)).catch(() => {}); }, []);
  const total = useMemo(() => products.reduce((sum, p) => sum + (cart[p.sku] || 0) * p.unitPriceCents, 0), [cart, products]);
  const update = (sku, delta) => setCart(current => ({ ...current, [sku]: Math.max(0, (current[sku] || 0) + delta) }));
  async function checkout() {
    const items = products.filter(p => cart[p.sku]).map(p => ({ sku: p.sku, quantity: cart[p.sku], unit_price_cents: p.unitPriceCents }));
    if (!items.length) return;
    const fingerprint = JSON.stringify(items);
    if (checkoutAttempt.current?.fingerprint !== fingerprint) checkoutAttempt.current = { fingerprint, key: Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, '0')).join('') };
    setLoading(true); setResult(null);
    try {
      const session = await fetch('/auth/session').then(response => response.json());
      if (!session.authenticated) { sessionStorage.setItem('pending-cart', JSON.stringify(cart)); location.assign('/auth/login'); return; }
      const response = await fetch(`${BFF_URL}/api/checkout`, {
        method: 'POST', headers: { 'content-type': 'application/json', 'idempotency-key': checkoutAttempt.current.key, 'x-csrf-token': session.csrfToken },
        body: JSON.stringify({ items })
      });
      let body = await response.json();
      if (!response.ok) {
        if (response.status < 500) checkoutAttempt.current = null;
        throw new Error(body.error || 'checkout_failed');
      }
      setResult(body);
      for (let attempt = 0; body.state === 'processing' && attempt < 90; attempt++) {
        await new Promise(resolve => setTimeout(resolve, 1000));
        const status = await fetch(`/api/checkouts/${encodeURIComponent(body.checkoutId || body.checkout_id)}`);
        body = await status.json();
        if (!status.ok) {
          if (status.status < 500) checkoutAttempt.current = null;
          throw new Error(body.error || 'checkout_failed');
        }
        setResult(body);
      }
      if (body.state !== 'processing') checkoutAttempt.current = null;
    } catch (error) { setResult({ error: error.message }); }
    finally { setLoading(false); }
  }
  return <main className="shell">
    <header><SessionControls /><p className="eyebrow">山嵐商店 / SERVICE POC</p><a className="back-link" href="/audit">open audit dashboard →</a><h1>一杯咖啡，三個服務。</h1><p className="lede">Next.js storefront → Desktop BFF → Order · Inventory · Payment.</p></header>
    <section className="layout"><div className="catalog">{products.map(product => <article className="product" key={product.sku}><div><span className="sku">{product.sku}</span><h2>{product.name}</h2><p>NT$ {product.unitPriceCents / 100}</p></div><div className="stepper"><button onClick={() => update(product.sku, -1)} aria-label={`減少 ${product.name}`}>−</button><span>{cart[product.sku] || 0}</span><button onClick={() => update(product.sku, 1)} aria-label={`增加 ${product.name}`}>＋</button></div></article>)}</div><aside className="checkout"><p className="eyebrow">CHECKOUT</p><div className="total"><span>總計</span><strong>NT$ {(total / 100).toFixed(0)}</strong></div><button className="primary" disabled={loading || total === 0} onClick={checkout}>{loading ? '建立訂單中…' : '建立待付款訂單'}</button>{result && <pre className={result.error ? 'result error' : 'result'}>{JSON.stringify(result, null, 2)}</pre>}</aside></section>
  </main>;
}
