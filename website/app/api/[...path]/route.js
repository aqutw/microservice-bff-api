import { proxyRequest } from '../../../lib/bff-proxy.mjs';

export const dynamic = 'force-dynamic';

async function forward(request, { params }) {
  return proxyRequest(request, (await params).path);
}

export { forward as GET, forward as POST, forward as OPTIONS };
