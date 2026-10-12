'use client';
import { useEffect, useState } from 'react';

export default function SessionControls({ returnTo = "/" }) {
  const [session, setSession] = useState(null);
  useEffect(() => { fetch('/auth/session').then(response => response.json()).then(setSession).catch(() => setSession({ authenticated: false })); }, []);
  if (!session) return <span aria-live="polite">讀取登入狀態…</span>;
  if (!session.authenticated) return <a href={`/auth/login?returnTo=${encodeURIComponent(returnTo)}`}>登入</a>;
  return <span>{session.user.name} · <button onClick={async () => {
    const response = await fetch('/auth/logout', { method: 'POST', headers: { 'x-csrf-token': session.csrfToken } });
    if (response.ok) location.assign((await response.json()).logoutURL || '/');
  }}>登出</button></span>;
}
