import http from 'node:http';
import { createServer } from './server.js';
const port = Number(process.env.PORT || 3002);
http.createServer(createServer('app')).listen(port, () => console.log(`app BFF listening on ${port}`));
