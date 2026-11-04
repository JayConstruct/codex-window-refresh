"""Offline Codex config-reader protocol used by executable client fixtures."""

CPA_URL = "http://127.0.0.1:8317/v1"
CONFIG_READER = '''
import json,os,sys
if 'app-server' in sys.argv[1:]:
    default={'model_provider':'fixture_proxy','model_providers':{'fixture_proxy':{'base_url':'http://127.0.0.1:8317/v1'}}}
    config=json.loads(os.environ.get('CWR_FIXTURE_CONFIG',json.dumps(default)))
    for line in sys.stdin:
        request=json.loads(line)
        if 'id' not in request:
            continue
        method=request['method']
        result=({'config':config} if method=='config/read' else
                {'requirements':json.loads(os.environ.get('CWR_FIXTURE_REQUIREMENTS','null'))} if method=='configRequirements/read' else {})
        print(json.dumps({'id':request['id'],'result':result}),flush=True)
    sys.exit(0)
'''
