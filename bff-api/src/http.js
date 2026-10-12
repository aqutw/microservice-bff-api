export function apiError(message, status, retryable = status >= 500) {
  return Object.assign(new Error(message), { status, retryable });
}

export async function request(base, path, options = {}, timeoutMs = Number(process.env.UPSTREAM_TIMEOUT_MS || 3000)) {
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1) throw apiError('invalid_upstream_timeout', 500);
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(`${base}${path}`, {
      ...options, signal: controller.signal,
      headers: { 'content-type': 'application/json', ...(options.headers || {}) }
    });
    const text = await response.text();
    let body;
    try { body = text ? JSON.parse(text) : {}; }
    catch { throw apiError('invalid_upstream_response', 502); }
    if (!response.ok) throw Object.assign(apiError(body.error || `upstream_${response.status}`, response.status), { body });
    return body;
  } catch (error) {
    if (controller.signal.aborted) throw apiError('upstream_timeout', 504);
    if (error.status) throw error;
    throw apiError('upstream_unavailable', 502);
  } finally { clearTimeout(timer); }
}

export function readBody(req, limit = 1024 * 1024) {
  return new Promise((resolve, reject) => {
    let size = 0, chunks = [], settled = false;
    function fail(error) {
      if (settled) return;
      settled = true;
      chunks = [];
      reject(error);
    }
    req.on('data', chunk => {
      if (settled) return;
      const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      size += bytes.length;
      if (size > limit) return fail(apiError('payload_too_large', 413, false));
      chunks.push(bytes);
    });
    req.on('end', () => {
      if (settled) return;
      try { const raw = Buffer.concat(chunks).toString('utf8'); resolve(raw ? JSON.parse(raw) : {}); }
      catch { reject(apiError('invalid_json', 400, false)); }
      settled = true; chunks = [];
    });
    req.on('error', () => fail(apiError('invalid_request_body', 400, false)));
    req.on('aborted', () => fail(apiError('request_aborted', 400, false)));
    if (Number(req.headers?.['content-length']) > limit) {
      fail(apiError('payload_too_large', 413, false));
      req.resume?.();
    }
  });
}
