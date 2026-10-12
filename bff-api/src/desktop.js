import https from 'node:https';
import { readFileSync } from 'node:fs';
import { createServer } from './server.js';
const port = Number(process.env.PORT || 3001);
https.createServer({ cert: readFileSync(process.env.SERVICE_TLS_CERT), key: readFileSync(process.env.SERVICE_TLS_KEY), minVersion: 'TLSv1.3' }, createServer('desktop')).listen(port, () => console.log(`desktop BFF listening on ${port}`));
