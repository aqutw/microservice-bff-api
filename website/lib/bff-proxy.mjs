export async function proxyRequest(request, segments) {
  const base = process.env.BFF_URL || 'http://localhost:3001';
  const target = new URL(`/api/${segments.map(encodeURIComponent).join('/')}`, base);
  target.search = new URL(request.url).search;
  const headers = new Headers();
  for (const name of ['content-type', 'authorization', 'idempotency-key', 'x-actor-id', 'x-correlation-id']) {
    if (request.headers.has(name)) headers.set(name, request.headers.get(name));
  }
  try {
    const upstream = await fetch(target, {
      method: request.method, headers, cache: 'no-store',
      body: ['GET', 'HEAD'].includes(request.method) ? undefined : request.body,
      duplex: 'half',
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(10000)])
    });
    return new Response(upstream.body, {
      status: upstream.status,
      headers: { 'content-type': upstream.headers.get('content-type') || 'application/json' }
    });
  } catch (error) {
    const timedOut = error.name === 'TimeoutError';
    return Response.json({ error: timedOut ? 'bff_timeout' : 'bff_unavailable' }, { status: timedOut ? 504 : 502 });
  }
}
