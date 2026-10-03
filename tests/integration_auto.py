#!/usr/bin/env python3
"""Run the auto-provider plugin against an isolated CPA and local mock upstreams.
Requires docker, PyYAML and a prebuilt dist/codex-policy.so. No production keys.
"""
import concurrent.futures
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import yaml

ROOT = Path(__file__).resolve().parents[1]
OBS = []
LOCK = threading.Lock()

class Mock(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        with LOCK: OBS.append({'supplier': self.server.supplier, 'body': body})
        if body.get('stream'):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            for delta, finish in [({'role':'assistant','content':'OK'}, None), ({}, 'stop')]:
                chunk = {'id':'chatcmpl-test','object':'chat.completion.chunk','created':int(time.time()),'model':body['model'],
                         'choices':[{'index':0,'delta':delta,'finish_reason':finish}]}
                self.wfile.write(('data: '+json.dumps(chunk)+'\n\n').encode())
                self.wfile.flush()
            self.wfile.write(b'data: [DONE]\n\n'); self.wfile.flush()
        else:
            payload = json.dumps({'id':'chatcmpl-test','object':'chat.completion','created':int(time.time()),'model':body['model'],
                                  'choices':[{'index':0,'message':{'role':'assistant','content':'OK'},'finish_reason':'stop'}],
                                  'usage':{'prompt_tokens':1,'completion_tokens':1,'total_tokens':2}}).encode()
            self.send_response(200); self.send_header('Content-Type','application/json')
            self.send_header('Content-Length',str(len(payload))); self.end_headers(); self.wfile.write(payload)

servers=[]
for supplier in ('a','b','unrelated'):
    server=ThreadingHTTPServer(('127.0.0.1',0),Mock); server.supplier=supplier
    threading.Thread(target=server.serve_forever,daemon=True).start();servers.append(server)

with socket.socket() as sock:
    sock.bind(('127.0.0.1',0)); proxy_port=sock.getsockname()[1]
base=f'http://127.0.0.1:{proxy_port}'
name='cpa-auto-'+uuid.uuid4().hex[:10]
report=[]

def curl(body, key='restricted-test', official=True, endpoint='/v1/responses'):
    args=['curl','-sS','--max-time','15','-w','\n%{http_code}',base+endpoint,
          '-H','Authorization: Bearer '+key,'-H','Content-Type: application/json']
    if official:
        args+=['-H','User-Agent: codex-tui/0.160.0','-H','Originator: codex_cli_rs',
               '-H','X-OpenAI-Internal-Codex-Responses-Lite: true']
    if body is not None: args+=['-d',json.dumps(body)]
    result=subprocess.run(args,text=True,capture_output=True)
    if result.returncode: return 0,result.stderr
    raw,code=result.stdout.rsplit('\n',1);return int(code),raw

def check(label,condition,detail=''):
    if not condition: raise AssertionError(label+': '+str(detail))
    report.append({'check':label,'result':'PASS'});print(json.dumps(report[-1]),flush=True)

try:
    with tempfile.TemporaryDirectory(prefix='cpa-auto-') as scratch:
        scratch=Path(scratch); (scratch/'auths').mkdir();(scratch/'plugins/linux/amd64').mkdir(parents=True)
        import shutil
        shutil.copy2(ROOT/'dist/codex-policy.so',scratch/'plugins/linux/amd64/codex-policy.so')
        providers=[{'name':s.supplier,'base-url':f'http://127.0.0.1:{s.server_port}/v1','disabled':s.supplier=='b',
                    'keys':[{'api-key':'mock-test'}],
                    'models':[{'name':'upstream-'+s.supplier,'alias':'gpt-6.1-sol' if s.supplier!='unrelated' else 'other-model'}]} for s in servers]
        plugin={'enabled':True,'enforce_all_protocols':True,'exempt_api_keys':['exempt-test'],
                'apply_to_non_codex':True,'log_format':'line',
                'model_map':{'rules':[{'from':'gpt-6-astra','to':'gpt-6.1-sol'}]}}
        config={'server':{'host':'127.0.0.1','port':proxy_port},'auth-dir':'/tmp/auto-auths',
                'access':{'api-keys':['restricted-test','exempt-test']},
                'plugins':{'enabled':True,'dir':'plugins','configs':{'codex-policy':plugin}},
                'api-keys':{'openai-compatibility':providers},'logging':{'debug':True}}
        path=scratch/'config.yaml';path.write_text(yaml.safe_dump(config,sort_keys=False))
        subprocess.run(['docker','run','-d','--name',name,'--network','host',
                        '-v',str(path)+':/CLIProxyAPI/config.yaml',
                        '-v',str(scratch/'plugins')+':/CLIProxyAPI/plugins',
                        os.environ.get('CPA_TEST_IMAGE','eceasy/cli-proxy-api:latest')],check=True,capture_output=True)
        def wait_models(owner=None,present=True):
            deadline=time.monotonic()+20
            while time.monotonic()<deadline:
                status,raw=curl(None,endpoint='/v1/models')
                if status==200:
                    models=json.loads(raw)['data'];target=[m for m in models if m['id']=='gpt-6.1-sol']
                    if (not present and not target) or (present and target and (owner is None or target[0].get('owned_by')==owner)):
                        return models
                time.sleep(.2)
            raise AssertionError('CPA model registry not updated')
        models=wait_models('a')
        check('source model absent from CPA registry',all(m['id']!='gpt-6-astra' for m in models))
        body={'model':'gpt-6-astra','input':'OK','reasoning':{'effort':'xhigh'},'stream':False}
        code,raw=curl(body);check('automatic supplier A and invisible model',code==200 and json.loads(raw)['model']=='gpt-6-astra',raw)
        with LOCK: last=OBS[-1]
        check('actual upstream A, target model and effort downgrade',last['supplier']=='a' and last['body']['model']=='upstream-a' and last['body'].get('reasoning_effort')=='medium',last)
        code,raw=curl(dict(body,stream=True))
        events=[json.loads(l[6:]) for l in raw.splitlines() if l.startswith('data: ') and l[6:]!='[DONE]']
        complete=[e for e in events if e.get('type')=='response.completed']
        check('SSE completed with client-visible model',code==200 and len(complete)==1 and complete[0]['response']['model']=='gpt-6-astra',raw)
        check('stream has no upstream model leak','upstream-a' not in raw and 'gpt-6.1-sol' not in raw)
        code,raw=curl(body,key='exempt-test');check('exemption bypasses mapping',code==400,raw)
        before=len(OBS);code,raw=curl(body,official=False)
        check('unofficial client denied before upstream',code==403 and len(OBS)==before,raw)
        # Supplier swap changes only CPA provider configuration, never the rule.
        providers[0]['disabled']=True;providers[1]['disabled']=False
        path.write_text(yaml.safe_dump(config,sort_keys=False));wait_models('b')
        code,raw=curl(body);check('supplier B selected after A disabled without rule edits',code==200,raw)
        with LOCK: last=OBS[-1]
        check('actual upstream B',last['supplier']=='b' and last['body']['model']=='upstream-b',last)
        # Responses endpoint needs input; verify the chat endpoint instead.
        chat={'model':'gpt-6-astra','messages':[{'role':'user','content':'OK'}],'reasoning_effort':'xhigh'}
        code,raw=curl(chat,endpoint='/v1/chat/completions')
        check('chat protocol mapping when enabled',code==200 and json.loads(raw)['model']=='gpt-6-astra',raw)
        with LOCK: last=OBS[-1]
        check('chat reasoning effort downgraded',last['body'].get('reasoning_effort')=='medium',last)
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            outcomes=list(pool.map(lambda _:curl(body),range(40)))
        check('40 requests, 8 concurrent, no unexpected response',all(c==200 and json.loads(r)['model']=='gpt-6-astra' for c,r in outcomes))
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            outcomes=list(pool.map(lambda _:curl(dict(body,stream=True)),range(20)))
        check('20 SSE requests, 4 concurrent, all completed',all(c==200 and r.count('event: response.completed')==1 and 'upstream-b' not in r for c,r in outcomes))
        providers[1]['disabled']=True
        path.write_text(yaml.safe_dump(config,sort_keys=False));wait_models(present=False)
        before=len(OBS);code,raw=curl(body)
        check('no target supplier preserves HTTP 400 and names target',code==400 and 'gpt-6.1-sol' in raw,raw)
        check('unrelated supplier never selected',len(OBS)==before)
        # Explicit provider mode remains supported.
        providers[1]['disabled']=False;plugin['model_map']['provider']='openai-compatible-b'
        path.write_text(yaml.safe_dump(config,sort_keys=False));wait_models('b');time.sleep(.5)
        code,raw=curl(body);check('explicit provider backwards compatibility',code==200,raw)
        logs=subprocess.run(['docker','logs',name],text=True,capture_output=True)
        check('no panic or plugin fuse','panic:' not in logs.stdout+logs.stderr and 'plugin fused' not in logs.stdout+logs.stderr)
        if os.environ.get('CPA_TEST_REPORT'):
            Path(os.environ['CPA_TEST_REPORT']).write_text(json.dumps(report,indent=2)+'\n')
except Exception:
    logs=subprocess.run(['docker','logs','--tail','70',name],text=True,capture_output=True)
    print(logs.stdout+logs.stderr)
    raise
finally:
    subprocess.run(['docker','rm','-f',name],capture_output=True)
    for s in servers:s.shutdown();s.server_close()
