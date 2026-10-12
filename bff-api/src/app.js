import https from 'node:https';
import { readFileSync } from 'node:fs';
import { createServer } from './server.js';
const port = Number(process.env.PORT || 3002);
https.createServer({ cert: readFileSync(process.env.SERVICE_TLS_CERT), key: readFileSync(process.env.SERVICE_TLS_KEY), minVersion: 'TLSv1.3' }, createServer('app')).listen(port, () => console.log(`app BFF listening on ${port}`));
