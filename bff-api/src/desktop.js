import http from 'node:http';
import { createServer } from './server.js';
const port = Number(process.env.PORT || 3001);
http.createServer(createServer('desktop')).listen(port, () => console.log(`desktop BFF listening on ${port}`));
