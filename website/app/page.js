'use client';
import { useEffect, useMemo, useState } from 'react';

const BFF_URL = process.env.NEXT_PUBLIC_BFF_URL || 'http://localhost:3001';
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
  useEffect(() => { fetch(`${BFF_URL}/api/catalog`).then(r => r.ok ? r.json() : null).then(data => data?.products && setProducts(data.products)).catch(() => {}); }, []);
  const total = useMemo(() => products.reduce((sum, p) => sum + (cart[p.sku] || 0) * p.unitPriceCents, 0), [cart, products]);
  const update = (sku, delta) => setCart(current => ({ ...current, [sku]: Math.max(0, (current[sku] || 0) + delta) }));
  async function checkout() {
    const items = products.filter(p => cart[p.sku]).map(p => ({ sku: p.sku, quantity: cart[p.sku], unit_price_cents: p.unitPriceCents }));
    if (!items.length) return;
    setLoading(true); setResult(null);
    try { const response = await fetch(`${BFF_URL}/api/checkout`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ user_id: 'web-demo-user', items }) }); const body = await response.json(); setResult(response.ok ? body : { error: body.error || 'checkout_failed' }); } catch (error) { setResult({ error: error.message }); } finally { setLoading(false); }
  }
  return <main className="shell">
    <header><p className="eyebrow">山嵐商店 / SERVICE POC</p><a className="back-link" href="/audit">open audit dashboard →</a><h1>一杯咖啡，三個服務。</h1><p className="lede">Next.js storefront → Desktop BFF → Order · Inventory · Payment.</p></header>
    <section className="layout"><div className="catalog">{products.map(product => <article className="product" key={product.sku}><div><span className="sku">{product.sku}</span><h2>{product.name}</h2><p>NT$ {product.unitPriceCents / 100}</p></div><div className="stepper"><button onClick={() => update(product.sku, -1)} aria-label={`減少 ${product.name}`}>−</button><span>{cart[product.sku] || 0}</span><button onClick={() => update(product.sku, 1)} aria-label={`增加 ${product.name}`}>＋</button></div></article>)}</div><aside className="checkout"><p className="eyebrow">CHECKOUT</p><div className="total"><span>總計</span><strong>NT$ {(total / 100).toFixed(0)}</strong></div><button className="primary" disabled={loading || total === 0} onClick={checkout}>{loading ? '建立訂單中…' : '建立待付款訂單'}</button>{result && <pre className={result.error ? 'result error' : 'result'}>{JSON.stringify(result, null, 2)}</pre>}</aside></section>
  </main>;
}
