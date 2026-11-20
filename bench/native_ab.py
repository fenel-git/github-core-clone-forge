#!/usr/bin/env python3
"""Native paired benchmarks only. Never publishes, submits or changes final_pick."""
import argparse, hashlib, json, os, pathlib, platform, re, shutil, subprocess, tarfile, time
BASELINE='75ddd531a113fb5e5dfc745594e74d280c20e2d1'
ORDER=('A1','B1','B2','A2','A3','B3')
def command(args,cwd=None,env=None,log=None):
    proc=subprocess.run(args,cwd=cwd,env=env,text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=1800)
    if log: pathlib.Path(log).write_text(proc.stdout)
    if proc.returncode: raise RuntimeError(f'{args[0]} failed ({proc.returncode}); see {log or proc.stdout[-1500:]}')
    return proc.stdout

def sha(path): return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
def stage(repo,ref,dest):
    resolved=command(['git','rev-parse','--verify',ref+'^{commit}'],cwd=repo).strip()
    archive=dest.parent/(dest.name+'.tar')
    command(['git','archive','--format=tar','-o',str(archive),resolved],cwd=repo)
    dest.mkdir()
    with tarfile.open(archive) as tf: tf.extractall(dest,filter='data')
    archive.unlink()
    # Do not bundle a previous deployment binary in place of this native build.
    env=dict(os.environ,CGO_ENABLED='0',GOOS='linux',GOARCH='amd64')
    for k in ('GOMAXPROCS','GOGC','GOMEMLIMIT'):env.pop(k,None)
    command(['go','test','./...'],cwd=dest,env=env,log=dest.parent/(dest.name+'-tests.log'))
    command(['go','vet','./...'],cwd=dest,env=env,log=dest.parent/(dest.name+'-vet.log'))
    command(['go','build','-trimpath','-buildvcs=false','-ldflags=-s -w -buildid=','-o','forge-linux-amd64','.'],cwd=dest,env=env,log=dest.parent/(dest.name+'-build.log'))
    return {'commit':resolved,'binary_sha256':sha(dest/'forge-linux-amd64'),'source_files':{str(p.relative_to(dest)):sha(p) for p in dest.rglob('*') if p.is_file()}}

def load_grade(home):
    runs=list((home/'.makefun/trials/github-core-clone-01').glob('*'))
    if len(runs)!=1:raise RuntimeError(f'Expected one grade run, got {len(runs)} under {home}')
    root=runs[0];rows=[]
    for i in range(3):
        trial=root/f'trial-{i}'
        states=list(trial.rglob('state.json'));perfs=list(trial.rglob('perf.json'))
        # Prefer the live state's documented export, not a replay copy.
        state=trial/'state/state.json';perf=trial/'state/perf.json'
        if not state.is_file() or not perf.is_file(): raise RuntimeError(f'Missing live state/perf files in {trial}')
        st=json.loads(state.read_text());pf=json.loads(perf.read_text())
        counts=st.get('counts',{});passed=counts.get('passed');total=counts.get('total')
        if passed!=120 or total!=120:raise RuntimeError(f'Conformance {passed}/{total} in {trial}')
        if len(pf.get('workloads',[]))!=4 or any(w.get('error') for w in pf['workloads']):raise RuntimeError(f'Workload failure in {trial}')
        rows.append({'trial':i,'state_sha256':sha(state),'passed':passed,'total':total,'perf':pf})
    return {'run_dir':str(root),'trials':rows}

def compare(runs,calibration=False):
    pairs=[]
    for n in (1,2,3):
        a,b=runs['A'+str(n)],runs['B'+str(n)]
        for x,y in zip(a['trials'],b['trials']):
            aw={w['name']:w for w in x['perf']['workloads']};bw={w['name']:w for w in y['perf']['workloads']}
            pairs.append({'pair':n,'trial':x['trial'],'state_identical':x['state_sha256']==y['state_sha256'],'composite_ratio':y['perf']['perf_p50_us']/x['perf']['perf_p50_us'],'workload_ratios':{k:bw[k]['p50_ms']/aw[k]['p50_ms'] for k in aw}})
    return {'calibration_only':calibration,'pairs':pairs,'all_states_identical':all(p['state_identical'] for p in pairs),'consistent_composite_win':all(p['composite_ratio']<1 for p in pairs),'no_workload_regression':all(v<=1 for p in pairs for v in p['workload_ratios'].values()),'official_preflight':'NOT_RUN','promotion_allowed':False,'reason':'This runner cannot publish. Post-cutoff official preflight, genuine provenance and owner review remain mandatory.'}

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--repo',type=pathlib.Path,required=True);ap.add_argument('--kit',type=pathlib.Path,required=True);ap.add_argument('--candidate',required=True);ap.add_argument('--output',type=pathlib.Path,required=True);ap.add_argument('--calibration',action='store_true');ap.add_argument('--single-core',action='store_true');args=ap.parse_args()
    if platform.system()!='Linux' or platform.machine() not in ('x86_64','amd64'):raise RuntimeError('Requires native Linux x86_64, not Termux/emulation')
    repo,kit,out=args.repo.resolve(),args.kit.resolve(),args.output.resolve();out.mkdir(parents=True,exist_ok=False)
    cli=kit/'cli/makefun.mjs';manifest=kit/'kit.json'
    if not cli.is_file() or not manifest.is_file():raise RuntimeError('Official kit is missing')
    spec=json.loads(manifest.read_text())
    for f in spec['files']:
        if sha(kit/f['path'])!=f['sha256']:raise RuntimeError('Kit hash mismatch: '+f['path'])
    allowed_cpus=sorted(os.sched_getaffinity(0));selected_cpu=allowed_cpus[0] if args.single_core else None
    meta={'baseline':BASELINE,'platform':platform.platform(),'machine':platform.machine(),'node':command(['node','--version']).strip(),'go':command(['go','version']).strip(),'kit_manifest_sha256':sha(manifest),'order':ORDER,'resource_mode':'shared-single-core-stress' if args.single_core else 'unrestricted','allowed_cpus':allowed_cpus,'selected_cpu':selected_cpu,'gomaxprocs':1 if args.single_core else 'default','io_throttling':'none; no production quota or gVisor equivalence claimed'}
    for label,ref in [('A',BASELINE),('B',args.candidate)]:meta[label]=stage(repo,ref,out/label)
    same=meta['A']['commit']==meta['B']['commit']
    if same and not args.calibration:raise RuntimeError('Identical baseline and candidate: use --calibration for A/A only')
    (out/'manifest.json').write_text(json.dumps(meta,indent=2));runs={}
    for label in ORDER:
        home=out/(label+'-home');home.mkdir();env=dict(os.environ,HOME=str(home))
        for k in list(env):
            if k.startswith(('OPENAI_','ANTHROPIC_','MAKEFUN_','GH_','GITHUB_TOKEN')) or k in ('GOMAXPROCS','GOGC','GOMEMLIMIT'):env.pop(k,None)
        grade_cmd=['node',str(cli),'grade-local','--kit',str(kit),'--bot',str(out/label[0]),'--driver','process','--instances','3']
        if args.single_core:
            env['GOMAXPROCS']='1'
            evidence=command(['taskset','--cpu-list',str(selected_cpu),'python3','-c','import os,json;print(json.dumps({"affinity":sorted(os.sched_getaffinity(0)),"GOMAXPROCS":os.environ.get("GOMAXPROCS")}))'],env=env)
            measured=json.loads(evidence);assert measured=={'affinity':[selected_cpu],'GOMAXPROCS':'1'},measured
            (out/(label+'-resource.json')).write_text(evidence)
            grade_cmd=['taskset','--cpu-list',str(selected_cpu)]+grade_cmd
        text=command(grade_cmd,cwd=kit,env=env,log=out/(label+'.log'))
        if len(re.findall(r'replay\s+matched:',text))!=3 or 'L1 regrade: reproduced byte for byte' not in text:raise RuntimeError('Replay/L1 verification incomplete for '+label)
        runs[label]=load_grade(home)
        binary=out/label[0]/'forge-linux-amd64'
        if sha(binary)!=meta[label[0]]['binary_sha256']:raise RuntimeError('Binary drift: '+label)
        (out/'runs.json').write_text(json.dumps(runs,indent=2))
    report=compare(runs,same);(out/'comparison.json').write_text(json.dumps(report,indent=2));print(json.dumps(report,indent=2))
    if not report['all_states_identical']:raise RuntimeError('Canonical state parity failed')
    if not same and not(report['consistent_composite_win'] and report['no_workload_regression']):raise RuntimeError('Performance gates failed; do not promote')
if __name__=='__main__':main()
