import { GitStore } from './git.ts';

export type Reply = {status:number; body:any; headers?:Record<string,string>};
type File = {sha:string; mode:string};
type Files = Map<string,File>;
type Repo = {data:any; git:GitStore; labels:Map<string,any>; issues:Map<number,any>; pulls:Map<number,any>; comments:Map<number,any[]>; number:number};
const out = (status:number, body:any, headers?:Record<string,string>):Reply => ({status,body,headers});
const error = (status:number, message:string):Reply => out(status,{message});
const missing = () => error(404,'Not Found');
const ref = (branch:string) => 'refs/heads/'+branch;
const key = (s:string) => s.toLowerCase();
const shaEq = (a?:File,b?:File) => a?.sha===b?.sha && a?.mode===b?.mode;
const isObject = (v:any) => v!==null && typeof v==='object' && !Array.isArray(v);
const str = (v:any, fallback=''):string => typeof v==='string' ? v : fallback;
const refName = (s:string) => s && !s.includes('..') && !s.includes('@{') && !/[ ~^:?*\x00- \x7f\\]/.test(s) && s.split('/').every(c=>c && !c.startsWith('.') && !c.endsWith('.') && !c.endsWith('.lock'));
const validPath = (s:string) => s && !s.includes('\0') && s.split('/').every(c=>c && c!=='.' && c!=='..' && key(c)!=='.git');

export class Forge {
  viewer:any;
  private readonly issueBytes = new WeakMap<object, Buffer>();
  private readonly issueListBytes = new WeakMap<Repo, Buffer>();
  issueWire(owner:string,name:string,number:number|undefined):Buffer|null|undefined {
    const repo=this.repo(owner,name);
    if(!repo)return null;
    if(number!==undefined){
      const issue=repo.issues.get(number);
      if(!issue)return null;
      let wire=this.issueBytes.get(issue);
      if(!wire){wire=Buffer.from(JSON.stringify(issue));this.issueBytes.set(issue,wire);}
      return wire;
    }
    let wire=this.issueListBytes.get(repo);
    if(wire)return wire;
    // The default page has no Link unless it overflows; let the generic dispatcher paginate.
    if(repo.issues.size>30)return undefined;
    const rows=[...repo.issues.values()].filter(i=>i.state==='open').reverse();
    wire=Buffer.from(JSON.stringify(rows));this.issueListBytes.set(repo,wire);return wire;
  }
  private invalidateIssue(repo:Repo,issue?:object){
    if(issue)this.issueBytes.delete(issue);
    this.issueListBytes.delete(repo);
  }
  repos = new Map<string,Repo>();
  ids = new Map<string|number,any>();
  serial=1;
  instant:number;
  origin='http://localhost';
  constructor(login:string, time:string) {
    const parsed=Date.parse(time);
    this.instant=Number.isFinite(parsed) ? parsed : 946684800000;
    this.viewer={login,id:1,node_id:'U_1',type:'User',site_admin:false,name:login};
  }
  private id(prefix:string) {const id=++this.serial; return {id,node_id:prefix+'_'+id};}
  private stamp() {return new Date(this.instant+this.serial*1000).toISOString();}
  private remember(data:any) {this.ids.set(data.id,data);this.ids.set(String(data.id),data);this.ids.set(data.node_id,data);return data;}
  repoById(id:string|number) {const data=this.ids.get(id); return data?.full_name && this.repos.get(key(data.name))?.data===data ? data : undefined;}
  issueById(id:string|number) {const data=this.ids.get(id); return data?.number && !data?.head ? data : undefined;}
  repo(owner:string,name:string) {return key(owner)===key(this.viewer.login) ? this.repos.get(key(name)) : undefined;}
  private page(url:URL,items:any[],fallback=30) {
    const size=Math.max(1,Math.min(100,Math.floor(Number(url.searchParams.get('per_page'))||fallback)));
    const page=Math.max(1,Math.floor(Number(url.searchParams.get('page'))||1));
    const start=(page-1)*size;
    const values=items.slice(start,start+size);
    if(start+size>=items.length) return out(200,values);
    url.searchParams.set('page',String(page+1));
    return out(200,values,{link:`<${url.href}>; rel="next"`});
  }
  private branchTip(repo:Repo, name:string) {
    const refs=repo.git.refs;
    return refs.get(ref(name)) ?? refs.get(name) ?? refs.get('refs/tags/'+name) ?? (repo.git.read(name)?.type==='commit' ? name : undefined);
  }
  request(method:string,path:string|URL,body?:any):Reply {
    let url:URL, p:string[];
    try {url=typeof path==='string'?new URL(path,this.origin):path; p=url.pathname.split('/').slice(1).map(decodeURIComponent); while(p.at(-1)==='')p.pop();} catch {return error(400,'Bad Request');}
    if(p[0]!=='api'||p[1]!=='v3')return missing();
    if(body!==undefined && !isObject(body))return error(400,'Expected a JSON object');
    const is= (n:number,s:string) => p.length===n && p[n-1]===s;
    if(p.length===2 && method==='GET')return out(200,{current_user_url:this.origin+'/api/v3/user',repository_url:this.origin+'/api/v3/repos/{owner}/{repo}'},{'x-oauth-scopes':'repo, read:org'});
    if(is(3,'meta')&&method==='GET')return out(200,{installed_version:'3.18.0'});
    if(is(3,'user')&&method==='GET')return out(200,this.viewer);
    if(is(4,'repos')&&p[2]==='user') {
      if(method==='GET')return this.page(url,[...this.repos.values()].map(r=>r.data));
      if(method==='POST') {
        const name=str(body?.name);
        if(!/^[a-z0-9_.-]{1,100}$/i.test(name)||name==='.'||name==='..'||this.repos.has(key(name)))return error(422,'Repository name is invalid or already exists');
        const owner=this.viewer;
        const data=this.remember({...this.id('R'),name,full_name:owner.login+'/'+name,owner,private:body.private===true,description:str(body.description),default_branch:'main',has_issues:true,fork:false,archived:false,permissions:{admin:true,maintain:true,push:true,triage:true,pull:true},created_at:this.stamp()});
        const git=new GitStore();
        if(body.auto_init===true) {
          const sha=git.write('blob',Buffer.from('# '+name+'\n'));
          git.refs.set(ref('main'),git.commit(new Map([['README.md',{sha,mode:'100644'}]]),[],'Initial commit'));
        }
        this.repos.set(key(name),{data,git,labels:new Map(),issues:new Map(),pulls:new Map(),comments:new Map(),number:0});
        return out(201,data);
      }
    }
    if(p[2]!=='repos'||p.length<5)return missing();
    const r=this.repo(p[3],p[4]); if(!r)return missing();
    const git=r.git, defaultBranch=r.data.default_branch;
    if(p.length===5){
      if(method==='GET')return out(200,r.data);
      if(method==='DELETE'){this.repos.delete(key(p[4])); for(const [i,item] of this.ids)if(item===r.data||item.repository_url===this.repoPath(r))this.ids.delete(i); return out(204,null);}
      if(method==='PATCH'){if(typeof body?.description==='string')r.data.description=body.description;if(typeof body?.has_issues==='boolean')r.data.has_issues=body.has_issues;return out(200,r.data);}
    }
    if(p.length===7&&p[5]==='git'&&p[6]==='refs'&&method==='POST'){
      const name=str(body?.ref), sha=str(body?.sha);
      if(!name.startsWith('refs/heads/')||!refName(name)||git.read(sha)?.type!=='commit'||git.refs.has(name))return error(422,'Invalid reference');
      git.refs.set(name,sha);return out(201,{ref:name,object:{sha,type:'commit'}});
    }
    if(is(6,'branches')&&method==='GET')return this.page(url,[...git.refs].filter(([k])=>k.startsWith('refs/heads/')).map(([k,sha])=>({name:k.slice(11),protected:false,commit:{sha}})));
    if(is(6,'commits')&&method==='GET'){
      const sha=this.branchTip(r,url.searchParams.get('sha')??defaultBranch);
      return sha?this.page(url,git.history(sha).map(c=>({sha:c.sha,commit:{message:c.message}}))):error(409,'Git Repository is empty');
    }
    if(p[5]==='contents')return this.contents(r,p.slice(6).join('/'),method,url,body);
    if(is(6,'labels')&&method==='GET')return this.page(url,[...r.labels.values()]);
    if(is(6,'labels')&&method==='POST'){
      const name=str(body?.name); if(!name||r.labels.has(key(name)))return error(422,'Invalid or duplicate label');
      const label={...this.id('LA'),name,color:str(body.color,'ededed'),description:str(body.description),default:false};r.labels.set(key(name),label);return out(201,label);
    }
    if(p[5]==='issues')return this.issues(r,p.slice(6),method,url,body);
    if(p[5]==='pulls')return this.pulls(r,p.slice(6),method,url,body);
    return missing();
  }
  private file(r:Repo,path:string,obj:File) {
    const bytes=r.git.read(obj.sha)?.data ?? Buffer.alloc(0);
    return {type:'file',name:path.split('/').at(-1),path,sha:obj.sha,mode:obj.mode,size:bytes.length,encoding:'base64',content:bytes.toString('base64')};
  }
  private contents(r:Repo,path:string,method:string,url:URL,body:any):Reply {
    if(path&&!validPath(path))return error(422,'Invalid path');
    const git=r.git, branch=method==='GET'?(url.searchParams.get('ref')??r.data.default_branch):str(body?.branch,r.data.default_branch), sha=this.branchTip(r,branch);
    if(!sha && (branch!==r.data.default_branch || method!=='PUT'))return missing();
    const files=sha?git.files(sha):new Map<string,File>();
    const old=files.get(path);
    if(method==='GET') {
      if(old)return out(200,this.file(r,path,old));
      const prefix=path?path+'/':'', list=new Map<string,any>();
      for(const [name,f] of files) if(name.startsWith(prefix)) {
        const parts=name.slice(prefix.length).split('/');
        if(parts.length===1)list.set(parts[0],this.file(r,name,f));
        else if(!list.has(parts[0]))list.set(parts[0],{type:'dir',name:parts[0],path:prefix+parts[0],size:0});
      }
      return list.size ? out(200,[...list.values()].sort((a,b)=>a.name<b.name?-1:a.name>b.name?1:0)):missing();
    }
    if((method!=='PUT'&&method!=='DELETE')||!path||!refName(branch)||!isObject(body))return error(422,'Invalid content request');
    if(method==='DELETE'&&!old)return missing();
    if((old||body.sha!==undefined) && body.sha!==old?.sha)return error(409,'SHA does not match current file');
    if(method==='PUT'){
      const content=typeof body.content==='string'?body.content.replace(/\s+/g,''):null;
      if(content===null||!(/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}(?:==)?|[A-Za-z0-9+/]{3}=?|)$/.test(content)))return error(422,'Invalid base64 content');
      for(const name of files.keys()) if(name!==path&&(name.startsWith(path+'/')||path.startsWith(name+'/')))return error(409,'File and directory conflict');
      files.set(path,{sha:git.write('blob',Buffer.from(content,'base64')),mode:old?.mode??'100644'});
    } else files.delete(path);
    const commit=git.commit(files,sha?[sha]:[],str(body.message,method==='DELETE'?'Delete '+path:'Update '+path));
    git.refs.set(ref(branch),commit);
    return method==='DELETE'?out(200,{content:null,commit:{sha:commit}}):out(old?200:201,{content:this.file(r,path,files.get(path)!),commit:{sha:commit}});
  }
  private repoPath(r:Repo){return '/api/v3/repos/'+encodeURIComponent(r.data.owner.login)+'/'+encodeURIComponent(r.data.name);}
  private issues(r:Repo,p:string[],method:string,url:URL,body:any):Reply {
    if(!p.length){
      if(method==='GET'){
        const state=url.searchParams.get('state')??'open', asc=url.searchParams.get('direction')==='asc';
        const list=[...r.issues.values()].filter(i=>state==='all'||state===i.state);
        if(!asc)list.reverse();return this.page(url,list);
      }
      if(method==='POST'){
        if(typeof body?.title!=='string'||!body.title.trim())return error(422,'Title is required');
        const labels=(Array.isArray(body.labels)?body.labels:[]).map((l:any)=>r.labels.get(key(str(typeof l==='string'?l:l?.name))));
        if(labels.some((x:any)=>!x))return error(422,'Unknown label');
        const id=this.id('I'), stamp=this.stamp(), repository_url=this.repoPath(r), number=++r.number;
        const item=this.remember({...id,number,title:body.title,body:str(body.body),state:'open',labels,comments:0,user:this.viewer,created_at:stamp,updated_at:stamp,closed_at:null,repository_url,url:repository_url+'/issues/'+number});
        r.issues.set(number,item);this.invalidateIssue(r);return out(201,item);
      }
    }
    const issue=r.issues.get(Number(p[0])); if(!issue)return missing();
    if(p.length===1){
      if(method==='GET')return out(200,issue);
      if(method==='PATCH'){
        if(typeof body?.title==='string')issue.title=body.title;
        if(typeof body?.body==='string')issue.body=body.body;
        if(body?.state==='closed'||body?.state==='open'){issue.state=body.state;issue.closed_at=body.state==='closed'?this.stamp():null;}
        issue.updated_at=this.stamp();this.invalidateIssue(r,issue);return out(200,issue);
      }
    }
    if(p.length===2&&p[1]==='comments'){
      const list=r.comments.get(issue.number)??[];
      if(method==='GET')return this.page(url,list);
      if(method==='POST'){
        if(typeof body?.body!=='string'||!body.body.trim())return error(422,'Comment body is required');
        const id=this.id('IC'), created_at=this.stamp();const item={...id,body:body.body,user:this.viewer,created_at,updated_at:created_at};list.push(item);r.comments.set(issue.number,list);issue.comments=list.length;this.invalidateIssue(r,issue);return out(201,item);
      }
    }
    return missing();
  }
  private diff(r:Repo,base:string,head:string) {
    const history=r.git.history(base), reachable=new Set(r.git.history(head).map(c=>c.sha));
    const ancestor=history.find(c=>reachable.has(c.sha))?.sha;
    const before=ancestor?r.git.files(ancestor):new Map<string,File>();
    const after=r.git.files(head);
    const current=r.git.files(base);
    const paths=[...new Set([...before.keys(),...after.keys()])].sort();
    const diff=paths.filter(path=>!shaEq(before.get(path),after.get(path))).map(path=>({path,old:before.get(path),target:after.get(path),current:current.get(path)}));
    return {diff,current,conflict:diff.some(d=>!shaEq(d.old,d.current)&&!shaEq(d.target,d.current))};
  }
  private pulls(r:Repo,p:string[],method:string,url:URL,body:any):Reply {
    if(!p.length){
      if(method==='GET'){
        const state=url.searchParams.get('state')??'open';
        return this.page(url,[...r.pulls.values()].reverse().filter(i=>state==='all'||state===i.state));
      }
      if(method==='POST'){
        const rawHead=str(body?.head), head=rawHead.includes(':')?rawHead.slice(rawHead.indexOf(':')+1):rawHead, base=str(body?.base);
        const hs=r.git.refs.get(ref(head)), bs=r.git.refs.get(ref(base));
        if(!hs||!bs||head===base||!str(body?.title).trim())return error(422,'Invalid head, base or title');
        const id=this.id('PR'), stamp=this.stamp(), repository_url=this.repoPath(r), number=++r.number;
        const side=(name:string,sha:string)=>({ref:name,sha,label:this.viewer.login+':'+name,repo:r.data,user:this.viewer});
        const item=this.remember({...id,number,title:body.title,body:str(body.body),head:side(head,hs),base:side(base,bs),state:'open',merged:false,mergeable:!this.diff(r,bs,hs).conflict,merge_commit_sha:null,user:this.viewer,created_at:stamp,updated_at:stamp,closed_at:null,merged_at:null,repository_url,url:repository_url+'/pulls/'+number});
        r.pulls.set(number,item);return out(201,item);
      }
    }
    const pull=r.pulls.get(Number(p[0]));if(!pull)return missing();
    if(p.length===1&&method==='GET')return out(200,pull);
    if(p.length===2&&p[1]==='files'&&method==='GET'){
      const diff=this.diff(r,pull.base.sha,pull.head.sha).diff;
      const lines=(obj?:File)=>{const bytes=obj?r.git.read(obj.sha)?.data:undefined;if(!bytes?.length||bytes.includes(0))return [];const s=bytes.toString('utf8');return (s.endsWith('\n')?s.slice(0,-1):s).split('\n');};
      return this.page(url,diff.map(({path,old,target})=>{
        const a=lines(old),b=lines(target);let first=0,last=0;
        while(first<a.length&&first<b.length&&a[first]===b[first])first++;
        while(last<a.length-first&&last<b.length-first&&a[a.length-last-1]===b[b.length-last-1])last++;
        const deletions=a.length-first-last, additions=b.length-first-last;
        return {sha:target?.sha??old?.sha,filename:path,status:!old?'added':!target?'removed':'modified',additions,deletions,changes:additions+deletions};
      }));
    }
    if(p.length===2&&p[1]==='merge'&&method==='PUT'){
      if(pull.state!=='open')return error(405,'Pull request is closed');
      const base=r.git.refs.get(ref(pull.base.ref)),head=r.git.refs.get(ref(pull.head.ref));
      if(!base||!head)return error(405,'Branch missing');
      if(body?.sha!==undefined && body.sha!==head)return error(409,'Head changed');
      const {diff,current,conflict}=this.diff(r,base,head);
      if(conflict){pull.mergeable=false;return error(405,'Pull request is not mergeable');}
      for(const {path,target} of diff)if(target)current.set(path,target);else current.delete(path);
      const parents=body?.merge_method==='merge'?[base,head]:[base];
      const sha=r.git.commit(current,parents,str(body?.commit_title,pull.title+' (#'+pull.number+')'));
      r.git.refs.set(ref(pull.base.ref),sha);
      Object.assign(pull,{state:'closed',merged:true,mergeable:true,merge_commit_sha:sha,closed_at:this.stamp(),merged_at:this.stamp()});
      return out(200,{sha,merged:true,message:'Pull Request successfully merged'});
    }
    return missing();
  }
}
