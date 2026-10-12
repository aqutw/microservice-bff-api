import assert from 'node:assert/strict';
import test from 'node:test';
import http from 'node:http';
import { generateKeyPair, exportJWK, SignJWT, createRemoteJWKSet } from 'jose';
import { createAuthenticator } from '../src/auth.js';

test('JWKS rotation accepts a new key and provider outages fail closed', async () => {
 const first = await generateKeyPair('RS256'), second = await generateKeyPair('RS256');
 let keys = [{ ...await exportJWK(first.publicKey), kid: 'first' }], failed = false;
 const server = http.createServer((req,res) => { res.writeHead(failed ? 503 : 200, {'content-type':'application/json'});res.end(JSON.stringify({keys})); });
 await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
 try {
  const config = {issuer:'https://issuer.test',audience:'commerce',algorithms:['RS256']};
  const url = new URL(`http://127.0.0.1:${server.address().port}/keys`);
  const authenticate = createAuthenticator(config,createRemoteJWKSet(url,{cooldownDuration:0,cacheMaxAge:0}));
  async function make(pair,kid) {return new SignJWT({sub:'alice'}).setProtectedHeader({alg:'RS256',kid}).setIssuer(config.issuer).setAudience(config.audience).setIssuedAt().setExpirationTime('1m').sign(pair.privateKey)}
  const initial = await authenticate({headers:{authorization:`Bearer ${await make(first,'first')}`}});
  keys = [{...await exportJWK(second.publicKey),kid:'second'}];
  assert.equal((await authenticate({headers:{authorization:`Bearer ${await make(second,'second')}`}})).id,initial.id);
  await assert.rejects(authenticate({headers:{authorization:`Bearer ${await make(first,'first')}`}}), {status:401});
  failed = true;
  await assert.rejects(authenticate({headers:{authorization:`Bearer ${await make(second,'second')}`}}), {status:503});
 } finally {server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
});
