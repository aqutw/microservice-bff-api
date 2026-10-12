'use client';
import { useEffect, useState } from 'react';

const BFF_URL = '';

export default function AuditPage() {
  const [events, setEvents] = useState([]);
  const [service, setService] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  async function loadEvents(selectedService = service) {
    setLoading(true); setError('');
    try {
      const query = selectedService ? `?service=${encodeURIComponent(selectedService)}&limit=100` : '?limit=100';
      const response = await fetch(`${BFF_URL}/api/audit/events${query}`);
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || 'audit_load_failed');
      setEvents(body.events || []);
    } catch (loadError) { setError(loadError.message); } finally { setLoading(false); }
  }

  useEffect(() => { loadEvents(''); }, []);

  function changeService(value) { setService(value); loadEvents(value); }

  return <main className="audit-shell">
    <header className="audit-header"><div><a className="back-link" href="/">← storefront</a><p className="eyebrow">OPERATIONS / AUDIT</p><h1>事件紀錄</h1><p className="lede">由 Node.js BFF 聚合訂單、庫存與金流服務的 audit events。</p></div><label className="audit-filter">SERVICE<select value={service} onChange={event => changeService(event.target.value)}><option value="">全部服務</option><option value="order">order</option><option value="inventory">inventory</option><option value="payment">payment</option></select></label></header>
    {loading && <p className="audit-state">讀取事件中…</p>}
    {error && <p className="audit-state audit-error">{error}</p>}
    {!loading && !error && <section className="audit-table-wrap"><table className="audit-table"><thead><tr><th>時間</th><th>服務</th><th>事件</th><th>Aggregate</th><th>Actor</th><th>狀態</th></tr></thead><tbody>{events.map(event => <tr key={event.event_id}><td>{new Date(event.occurred_at).toLocaleString('zh-TW', { hour12: false })}</td><td><span className={`service-badge service-${event.service}`}>{event.service}</span></td><td><strong>{event.event_type}</strong><small>{event.action}</small></td><td><code>{event.aggregate_id}</code></td><td>{event.actor_id}<small>{event.correlation_id}</small></td><td><span className={`status-${event.status}`}>{event.status}</span></td></tr>)}</tbody></table>{events.length === 0 && <p className="audit-state">目前沒有事件。</p>}</section>}
  </main>;
}
