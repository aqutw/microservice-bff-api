// Fresh Compose project and volumes, without reading or overwriting existing credentials.
import { cpSync, mkdirSync, mkdtempSync, openSync, closeSync, rmSync, writeFileSync } from 'node:fs';
import { spawn, execFileSync } from 'node:child_process';
import { once } from 'node:events';
import net from 'node:net';
import { basename, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const repository = resolve(fileURLToPath(new URL('..', import.meta.url)));
mkdirSync(join(repository, 'artifacts'), { recursive: true });
const workspace = mkdtempSync(join(repository, 'artifacts/compose-sit-'));
const project = `commerce-gap-sit-${basename(workspace).slice(-6).toLowerCase()}`;
const sourceRevision=execFileSync('git',['rev-parse','HEAD'],{cwd:repository,encoding:'utf8'}).trim();
const log = join(workspace, 'run.log');
const env = { ...process.env, COMPOSE_PROJECT_NAME: project, COMPOSE_FILE: 'docker-compose.yml:docker-compose.dev.yml', COMPOSE_ENV_FILES: '.env.security', SECURITY_ENV_FILE: '.env.security' };
for(const key of Object.keys(env)) if(/^(AUTH_|OIDC_|SESSION_|SERVICE_|TEST_|ALICE_|BOB_|AUDITOR_|SIT_|PUBLIC_URL$|DEPLOYMENT_ENV$|ACCESS_TOKEN$)|_(INCOMING|OUTGOING)_KEYS$/.test(key)) delete env[key];
try{execFileSync('docker',['compose','version'],{stdio:'ignore'});env.COMPOSE_BIN='docker';}
catch{execFileSync('docker-compose',['version'],{stdio:'ignore'});env.COMPOSE_BIN='docker-compose';}
for (const key of ['DESKTOP_BFF_URL','APP_BFF_URL','ORDER_API_URL','INVENTORY_API_URL','PAYMENT_API_URL','AUDIT_API_URL','WEBSITE_URL']) delete env[key];
for (const name of ['services','scripts','bff-api','website','loadtest']) {
 cpSync(join(repository,name),join(workspace,name),{recursive:true,filter:path=>!path.split(/[\\/]/).some(part=>['node_modules','.next','data'].includes(part))});
}
for (const name of ['docker-compose.yml','docker-compose.dev.yml']) cpSync(join(repository,name),join(workspace,name));
async function command(executable,args) {
 if(executable==='docker' && args[0]==='compose' && env.COMPOSE_BIN!=='docker'){executable=env.COMPOSE_BIN;args=args.slice(1);}
 const fd=openSync(log,'a');
 try {
  const child=spawn(executable,args,{cwd:workspace,env,stdio:['ignore',fd,fd]});
  const [code]=await once(child,'exit');
  if(code!==0) throw new Error(`${executable} ${args.join(' ')} exited ${code}; diagnostics: ${log}`);
 } finally {closeSync(fd);}
}
const allocated=new Set();
async function port() {
 while(true) {
  const server=net.createServer();server.listen(0,'127.0.0.1');await once(server,'listening');
  const value=server.address().port;await new Promise(resolve=>server.close(resolve));
  if(!allocated.has(value)){allocated.add(value);return String(value);}
 }
}
let success=false;
try {
 for(const key of ['IDENTITY_PORT','WEBSITE_PORT','DESKTOP_PORT','APP_PORT','ORDER_PORT','INVENTORY_PORT','PAYMENT_PORT','AUDIT_PORT']) env[key]=await port();
 for(const [url,portKey] of Object.entries({DESKTOP_BFF_URL:'DESKTOP_PORT',APP_BFF_URL:'APP_PORT',ORDER_API_URL:'ORDER_PORT',INVENTORY_API_URL:'INVENTORY_PORT',PAYMENT_API_URL:'PAYMENT_PORT',AUDIT_API_URL:'AUDIT_PORT'})) env[url]=`https://localhost:${env[portKey]}`;
 env.WEBSITE_URL=`http://localhost:${env.WEBSITE_PORT}`;
 await command(process.execPath,['scripts/init-security.mjs']);
 env.NODE_EXTRA_CA_CERTS=join(workspace,'.security/pki/ca.pem');env.CURL_CA_BUNDLE=env.NODE_EXTRA_CA_CERTS;
 console.log(`Building isolated Compose SIT: ${project}`);
 await command('docker',['compose','up','--build','-d']);
 console.log('Running real Keycloak/OIDC, ownership, CSRF and recovery SIT');
 await command(process.execPath,['scripts/security-sit.mjs']);
 console.log('Running real commerce, outages and audit replay SIT');
 await command('bash',['scripts/sit.sh']);
 success=true;
 console.log(`Compose/OIDC SIT passed; report: ${workspace}`);
} finally {
 for(const args of [['compose','ps','--all'],['compose','logs','--no-color'],['compose','down','-v','--remove-orphans','--rmi','local']]) {
  try{await command('docker',args);}catch(error){console.error(error.message);}
 }
 writeFileSync(join(workspace,'result.json'),JSON.stringify({project,sourceRevision,success,checks:['security-sit.mjs','sit.sh']},null,2));
 // Preserve test logs/results, not the generated credentials or build contexts.
 for(const name of ['.security','.env.security','services','scripts','bff-api','website','loadtest','docker-compose.yml','docker-compose.dev.yml']) rmSync(join(workspace,name),{recursive:true,force:true});
}
