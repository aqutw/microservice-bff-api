process.env.SERVICE_NAME = 'desktop-bff';
process.env.SERVICE_OUTGOING_KEYS = JSON.stringify(Object.fromEntries(['order','payment','audit-worker'].map(peer => [peer, Buffer.alloc(32, 11).toString('base64url')])));
