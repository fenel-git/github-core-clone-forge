#!/usr/bin/env python3
"""Separate syscall audit of native server only; never used for A/B timings."""
import argparse, hashlib, json, os, pathlib, re, selectors, signal, subprocess, time

def run_audit(binary,kit,output,core):
    trace=output/(binary.parent.name+'-server.strace');env=dict(os.environ,MAKEFUN_SHOP_TOKEN='local-syscall-audit',MAKEFUN_FORGE_LOGIN='adaiversen',GOMAXPROCS='1')
    for k in list(env):
        if k.startswith(('OPENAI_','ANTHROPIC_','GH_','GITHUB_TOKEN')):env.pop(k,None)
    args=['taskset','--cpu-list',str(core),'strace','-f','-qq','-yy','-s','120','-e','trace=%file,%process,write,writev,pwrite64,fsync,fdatasync','-o',str(trace),str(binary),'--listen','127.0.0.1:0']
    log=(output/(binary.parent.name+'-audit-server.log')).open('w')
    proc=subprocess.Popen(args,cwd=binary.parent,env=env,stdout=subprocess.PIPE,stderr=log,start_new_session=True,text=True)
    try:
        assert proc.stdout is not None
        sel=selectors.DefaultSelector();sel.register(proc.stdout,selectors.EVENT_READ)
        if not sel.select(timeout=20):raise RuntimeError('server listen timed out')
        line=proc.stdout.readline();match=re.search(r'listening on port (\d+)',line)
        if not match:raise RuntimeError('server not ready: '+line)
        result=subprocess.run(['node',str(kit/'sdk/run.ts'),'--url','http://127.0.0.1:'+match.group(1),'--token','local-syscall-audit','--json','--perf'],cwd=kit,env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=180)
        (output/(binary.parent.name+'-audit-sdk.log')).write_text(result.stderr)
        data=json.loads(result.stdout)
        if result.returncode or data.get('counts',{}).get('passed')!=120:raise RuntimeError('syscall SDK conformance failed')
        if len(data.get('perf',{}).get('workloads',[]))!=4 or any(w.get('error') for w in data['perf']['workloads']):raise RuntimeError('audit workloads incomplete')
    finally:
        if proc.poll() is None:os.killpg(proc.pid,signal.SIGTERM)
        try:proc.wait(timeout=15)
        except subprocess.TimeoutExpired:os.killpg(proc.pid,signal.SIGKILL);proc.wait()
        log.close()
    text=trace.read_text();lines=text.splitlines()
    def matching(pattern):return [line for line in lines if re.search(pattern,line)]
    writable=matching(r'\b(?:open|openat|creat)\(.*(?:O_WRONLY|O_RDWR|O_CREAT|O_TRUNC)')
    regular_writes=matching(r'\b(?:write|writev|pwrite64)\(\d+</(?!dev/)[^>]*>')
    sync=matching(r'\b(?:fsync|fdatasync)\(')
    execs=matching(r'\bexecve(?:at)?\(')
    git_exec=[x for x in execs if re.search(r'[/"]git(?:[" -]|$)',x)]
    process_forks=matching(r'\b(?:fork|vfork)\(')
    return {'binary':str(binary),'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'scope':'server process and descendants including startup, full SDK and all four workloads; SDK process not traced','diagnostic_only':True,'sdk_passed':data['counts']['passed'],'sdk_workloads':[w['name'] for w in data['perf']['workloads']],'writable_file_open_attempts':len(writable),'regular_file_write_attempts':len(regular_writes),'fsync_fdatasync_attempts':len(sync),'exec_attempts':len(execs),'exec_samples':execs[:8],'git_exec_attempts':len(git_exec),'fork_vfork_attempts':len(process_forks),'write_open_samples':writable[:10],'regular_write_samples':regular_writes[:10],'trace_sha256':hashlib.sha256(trace.read_bytes()).hexdigest(),'trace_lines':len(lines)}

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--results',type=pathlib.Path,required=True);ap.add_argument('--kit',type=pathlib.Path,required=True);a=ap.parse_args();out=a.results.resolve();kit=a.kit.resolve();meta=json.loads((out/'manifest.json').read_text());core=meta['selected_cpu']
    audits={}
    for label in ['A','B']:audits[label]=run_audit(out/label/'forge-linux-amd64',kit,out,core)
    (out/'syscall-audit.json').write_text(json.dumps(audits,indent=2));print(json.dumps(audits,indent=2))
if __name__=='__main__':main()
