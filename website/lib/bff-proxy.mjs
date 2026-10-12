import { authSettings, readSession, validMutation } from './auth.mjs';
export async function proxyRequest(request, segments) {
  const base = process.env.BFF_URL || 'http://localhost:3001';
  const target = new URL(`/api/${segments.map(encodeURIComponent).join('/')}`, base);
  target.search = new URL(request.url).search;
  const headers = new Headers();
  for (const name of ['content-type', 'idempotency-key', 'x-correlation-id']) {
    if (request.headers.has(name)) headers.set(name, request.headers.get(name));
  }
  try {
    if (!(segments.length === 1 && segments[0] === 'catalog' && request.method === 'GET')) {
      const settings = authSettings();
      const session = await readSession(request, settings);
      if (!session) return Response.json({ error: 'unauthorized' }, { status: 401, headers: { 'cache-control': 'no-store' } });
      if (!['GET', 'HEAD'].includes(request.method) && !validMutation(request, session, settings)) return Response.json({ error: 'csrf_rejected' }, { status: 403 });
      headers.set('authorization', `Bearer ${session.accessToken}`);
    }
    const upstream = await fetch(target, {
      method: request.method, headers, cache: 'no-store',
      body: ['GET', 'HEAD'].includes(request.method) ? undefined : request.body,
      duplex: 'half',
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(10000)])
    });
    return new Response(upstream.body, {
      status: upstream.status,
      headers: { 'content-type': upstream.headers.get('content-type') || 'application/json', 'cache-control': 'no-store' }
    });
  } catch (error) {
    const timedOut = error.name === 'TimeoutError';
    return Response.json({ error: timedOut ? 'bff_timeout' : 'bff_unavailable' }, { status: timedOut ? 504 : 502 });
  }
}
