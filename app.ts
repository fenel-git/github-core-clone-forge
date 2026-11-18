import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { Forge, type Reply } from './store.ts';
import { graphql } from './graphql.ts';
import { mcp } from './mcp.ts';

const LIMIT = 32 * 1024 * 1024;
const EMPTY = Buffer.alloc(0);
const ISSUE_PATH = /^\/api\/v3\/repos\/([A-Za-z0-9_-][A-Za-z0-9_.-]*)\/([A-Za-z0-9_-][A-Za-z0-9_.-]*)\/issues(?:\/([1-9][0-9]*))?\/?$/;
const SIMPLE_HOST = /^(?:[A-Za-z0-9.-]+|\[::1\])(?::[0-9]{1,4})?$/;
const PORT_HOST = /^(?:[A-Za-z0-9.-]+|\[::1\]):([0-9]{5})$/;
function simpleHost(host:string):boolean {
  if(SIMPLE_HOST.test(host))return true;
  const match=PORT_HOST.exec(host);return !!match&&Number(match[1])<=65535;
}
const HEALTH = Buffer.from('{"ok":true}');
const BAD = (status:number,message:string):Reply => ({status,body:{message}});
const rpcError = (code:number,message:string) => ({jsonrpc:'2.0',id:null,error:{code,message}});

export function createApp() {
  const api = new Forge(process.env.MAKEFUN_FORGE_LOGIN || 'adaiversen', process.env.MAKEFUN_TRIAL_NOW || '');
  const token = process.env.MAKEFUN_SHOP_TOKEN || '';
  const bearer = Buffer.from('Bearer '+token);
  let sequence = 0;
  const authed = (req:IncomingMessage) => {
    if(!token)return false;
    const auth = req.headers.authorization;
    if (typeof auth !== 'string') return false;
    const value=Buffer.from(auth);
    if(value.length===bearer.length&&timingSafeEqual(value,bearer)) return true;
    // Git's HTTP transport also accepts the conventional username:PAT Basic form.
    if(!auth.startsWith('Basic ') || !req.url?.includes('.git/'))return false;
    const decoded=Buffer.from(auth.slice(6),'base64').toString();
    const colon=decoded.indexOf(':'); if(colon<0)return false;
    const supplied=Buffer.from('Bearer '+decoded.slice(colon+1));
    return supplied.length===bearer.length&&timingSafeEqual(supplied,bearer);
  };
  function send(res:ServerResponse, reply:Reply, id:string) {
    const body=reply.status===204||reply.status===202||reply.body===undefined ? '' : JSON.stringify(reply.body);
    res.writeHead(reply.status, {'content-type':'application/json; charset=utf-8','content-length':Buffer.byteLength(body),'x-github-request-id':id,...reply.headers});
    res.end(body);
  }
  function slurp(req:IncomingMessage):Promise<Buffer> {
    return new Promise((resolve,reject)=>{
      const chunks:Buffer[]=[]; let total=0, tooLarge=false;
      req.on('data',(chunk:Buffer)=>{
        total+=chunk.length;
        if(total>LIMIT){if(!tooLarge){tooLarge=true;chunks.length=0;reject(new RangeError('Request too large'));}}
        else chunks.push(chunk);
      });
      req.once('end',()=>{if(!tooLarge)resolve(chunks.length===0?EMPTY:chunks.length===1?chunks[0]:Buffer.concat(chunks,total));});
      req.once('error',reject);
      req.once('aborted',()=>reject(new Error('Request aborted')));
    });
  }
  const server=createServer({maxHeaderSize:16384},(req,res)=>{
    const requestId='FORGE-'+(++sequence).toString(36);
    const raw=req.url??'/', method=req.method??'GET';
    if(method==='GET'&&raw==='/healthz'){
      res.writeHead(200,{'content-type':'application/json; charset=utf-8','content-length':HEALTH.length});res.end(HEALTH);return;
    }
    if(!raw.startsWith('/')||raw.startsWith('//')){req.resume();send(res,BAD(400,'Bad Request'),requestId);return;}
    const host=req.headers.host??'localhost';
    const query=raw.indexOf('?');
    const pathname=query<0?raw:raw.slice(0,query);
    if(method==='GET' && pathname.startsWith('/api/v3/repos/') && simpleHost(host)){
      const issue=ISSUE_PATH.exec(pathname);
      // Query parameters affect list pagination/filtering, but not an individual issue.
      if(issue && (issue[3]!==undefined || query<0)){
        if(!authed(req)){req.resume();send(res,BAD(401,'Bad credentials'),requestId);return;}
        const bytes=api.issueWire(issue[1],issue[2],issue[3]===undefined?undefined:Number(issue[3]));
        if(bytes!==undefined){
          req.resume();
          if(bytes===null){send(res,BAD(404,'Not Found'),requestId);return;}
          res.writeHead(200,{'content-type':'application/json; charset=utf-8','content-length':bytes.length,'x-github-request-id':requestId});
          res.end(bytes);return;
        }
      }
    }
    const base='http://'+host;
    let url:URL;
    try{url=new URL(raw,base);api.origin=url.origin;}catch{req.resume();send(res,BAD(400,'Bad Request'),requestId);return;}
    const p=url.pathname;
    const isRest=p==='/api/v3'||p.startsWith('/api/v3/'), isGraph=p==='/api/graphql', isMcp=p==='/mcp';
    const match=!isRest&&!isGraph&&!isMcp ? /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/.exec(p):null;
    if(!isRest&&!isGraph&&!isMcp&&!match){req.resume();send(res,BAD(404,'Not Found'),requestId);return;}
    if(!authed(req)){
      req.resume();
      const answer=BAD(401,'Bad credentials');
      if(match)answer.headers={'www-authenticate':'Basic realm="Forge"'};
      send(res,answer,requestId);return;
    }
    if(isRest&&(method==='GET'||method==='HEAD')){
      req.resume();send(res,api.request(method,url),requestId);return;
    }
    async function route(){
      let bytes:Buffer;
      try{if(method==='GET'||method==='HEAD'){req.resume();bytes=EMPTY;}else bytes=await slurp(req);}catch(err){if(!res.destroyed)send(res,BAD(err instanceof RangeError?413:400,'Invalid request body'),requestId);return;}
      api.origin=url.origin;
      if(match){
        let owner:string,name:string;
        try{owner=decodeURIComponent(match[1]);name=decodeURIComponent(match[2]);}catch{send(res,BAD(400,'Bad path'),requestId);return;}
        const repo=api.repo(owner,name);
        if(!repo){send(res,BAD(404,'Not Found'),requestId);return;}
        await repo.git.http(req,res,url,match[3],bytes);return;
      }
      if((isGraph||isMcp)&&method!=='POST'){send(res,BAD(405,'Method Not Allowed'),requestId);return;}
      let body:any;
      if(bytes.length){
        try{body=JSON.parse(bytes.toString('utf8'));}
        catch{send(res,isMcp?{status:400,body:rpcError(-32700,'Parse error')}:BAD(400,'Problems parsing JSON'),requestId);return;}
      }
      if(isGraph)send(res,{status:200,body:graphql(body,api,url.origin)},requestId);
      else if(isMcp)send(res,mcp(body,api),requestId);
      else send(res,api.request(method,url,body),requestId);
    }
    void route().catch(()=>{
      if(!res.headersSent&&!res.destroyed)send(res,BAD(500,'Internal Server Error'),requestId);
      else if(!res.writableEnded)res.destroy();
    });
  });
  server.keepAliveTimeout=30_000;
  return server;
}
