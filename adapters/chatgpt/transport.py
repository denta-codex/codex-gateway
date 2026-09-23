"""Private stdio transport. Adapted from suphotP/chatgpt-api (see UPSTREAM-LICENSE).
Credentials arrive on stdin, never in argv, logs, or the filesystem.
"""
import json
import sys
import time
import uuid
from datetime import datetime
from urllib.parse import urlparse
from zoneinfo import ZoneInfo
from curl_cffi import requests
from proof import generate_proof_token
from stream import TextStream

BASE = 'https://chatgpt.com'
LIMIT = 8 * 1024 * 1024

def emit(value):
    print(json.dumps(value, separators=(',', ':')), flush=True)

class Failure(Exception):
    def __init__(self, code, status=None):
        self.code, self.status = code, status

def checked(response, stage):
    if not 200 <= response.status_code < 300:
        raise Failure(stage, response.status_code)
    return response

def events(lines):
    for line in lines:
        if isinstance(line, bytes):
            line = line.decode('utf-8')
        if len(line) > LIMIT:
            raise Failure('stream_limit')
        if line.startswith('data:'):
            data = line[5:].strip()
            if data == '[DONE]':
                yield {'type': 'done'}
            elif data:
                parsed = json.loads(data)
                for event in parsed if isinstance(parsed, list) else [parsed]:
                    if isinstance(event, dict):
                        yield event

def ws_events(item, topic):
    for catchup in item.get('reply', {}).get('catchups', []):
        yield from ws_events(catchup, topic)
    if item.get('type') == 'message' and item.get('topic_id') == topic:
        inner = item.get('payload', {}).get('payload', {})
        if inner.get('type') == 'done':
            yield {'type': 'done'}
        if isinstance(inner.get('encoded_item'), str):
            yield from events(inner['encoded_item'].splitlines())

def follow_topic(session, headers, topic):
    import websocket
    data = checked(session.get(BASE+'/backend-api/celsius/ws/user', headers=headers, timeout=30, allow_redirects=False), 'websocket_url').json()
    url = data.get('websocket_url', '')
    parsed = urlparse(url)
    if parsed.scheme != 'wss' or not (parsed.hostname == 'chatgpt.com' or (parsed.hostname or '').endswith('.chatgpt.com')):
        raise Failure('invalid_websocket_url')
    ws = websocket.create_connection(url, timeout=30, origin=BASE, redirect_limit=0)
    try:
        ws.send(json.dumps([{'id':1,'command':{'type':'connect','presence':{'type':'presence','state':'foreground'}}}, {'id':2,'command':{'type':'subscribe','topic_id':topic,'offset':'0'}}]))
        deadline = time.monotonic()+900
        while time.monotonic() < deadline:
            try:
                raw = ws.recv()
            except websocket.WebSocketTimeoutException:
                emit({'type':'heartbeat'})
                continue
            if not raw:
                raise Failure('websocket_closed')
            if len(raw) > LIMIT:
                raise Failure('stream_limit')
            rows = json.loads(raw)
            for item in rows if isinstance(rows,list) else [rows]:
                for event in ws_events(item,topic):
                    yield event
                    if event.get('type') in ('done','message_stream_complete'):
                        return
        raise Failure('websocket_timeout')
    finally:
        ws.close()

def main():
    data = json.loads(sys.stdin.readline(LIMIT+1))
    headers = {'authorization':'Bearer '+data['accessToken'], 'chatgpt-account-id':data['accountId'], 'origin':BASE, 'referer':BASE+'/', 'oai-device-id':str(uuid.uuid4())}
    session = requests.Session(impersonate='chrome')
    catalog = checked(session.get(BASE+'/backend-api/models',headers=headers,timeout=30,allow_redirects=False),'catalog').json()
    if data.get('operation') == 'catalog':
        emit({'type':'catalog','models':[{k:m.get(k) for k in ('slug','configurable_thinking_effort','thinking_efforts')} for m in catalog.get('models',[])], 'versions':catalog.get('versions',[])})
        return
    versions = [v for v in catalog.get('versions',[]) if v.get('id')=='latest']
    lane = data['lane']
    presets = versions[0].get('intelligence_presets',[]) if versions else []
    candidates = [p for p in presets if p.get('lane')==lane and p.get('preset_type','available')=='available']
    if not candidates:
        raise Failure('mode_unavailable')
    model = candidates[0]['model_slug']
    row = next((m for m in catalog.get('models',[]) if m.get('slug')==model),None)
    effort = data.get('effort')
    if row is None or (effort and effort not in [e.get('thinking_effort') for e in row.get('thinking_efforts',[])]):
        raise Failure('effort_unavailable')
    zone = ZoneInfo('America/New_York')
    offset = -int(datetime.now(zone).utcoffset().total_seconds() // 60)
    payload = {'action':'next','model':model,'parent_message_id':'client-created-root','timezone':'America/New_York','timezone_offset_min':offset,'conversation_mode':{'kind':'primary_assistant'},'system_hints':[],'supports_buffering':True,'supported_encodings':['v1'],'client_prepare_state':'none','history_and_training_disabled':True,'force_parallel_switch':'auto','paragen_cot_summary_display_override':'allow'}
    if effort:
        payload['thinking_effort']=effort
    prepared=checked(session.post(BASE+'/backend-api/f/conversation/prepare',headers=headers,json=payload,timeout=30,allow_redirects=False),'prepare').json()
    if prepared.get('conduit_token'):
        headers['x-conduit-token']=prepared['conduit_token']
    requirements=checked(session.post(BASE+'/backend-api/sentinel/chat-requirements',headers=headers,json={'p':None},timeout=30,allow_redirects=False),'requirements').json()
    if requirements.get('force_login'):
        raise Failure('login_required',401)
    if requirements.get('token'):
        headers['openai-sentinel-chat-requirements-token']=requirements['token']
    proof=requirements.get('proofofwork',{})
    if proof.get('required'):
        headers['openai-sentinel-proof-token']=generate_proof_token(True,proof.get('seed',''),proof.get('difficulty',''))
    payload.update({'client_prepare_state':'sent','messages':[{'id':str(uuid.uuid4()),'author':{'role':'user'},'content':{'content_type':'text','parts':[data['prompt']]}}]})
    emit({'type':'ready','model':model})
    if sys.stdin.readline().strip() != 'send':
        raise Failure('dispatch_not_authorized')
    headers['accept']='text/event-stream'
    response=checked(session.post(BASE+'/backend-api/f/conversation',headers=headers,json=payload,stream=True,timeout=900,allow_redirects=False),'inference')
    complete=False
    text_stream=TextStream()
    total=0
    topics=[]
    def consume(event):
        nonlocal complete, total
        kind=event.get('type')
        if event.get('error'):
            raise Failure('upstream_error')
        if kind in ('message_stream_complete','done'):
            complete=True
        if kind=='stream_handoff':
            topics.extend(o['topic_id'] for o in event.get('options',[]) if o.get('type')=='subscribe_ws_topic')
        for text in text_stream.consume(event):
            total+=len(text.encode())
            if total>LIMIT:
                raise Failure('output_limit')
            emit({'type':'text','text':text})
    try:
        for event in events(response.iter_lines()):
            consume(event)
        for topic in dict.fromkeys(topics):
            complete=False
            for event in follow_topic(session,headers,topic):
                consume(event)
        if not total:
            raise Failure('empty_response')
        if not complete:
            raise Failure('incomplete_stream')
        emit({'type':'done','model':model})
    finally:
        response.close()
        session.close()

if __name__=='__main__':
    try:
        main()
    except Failure as e:
        emit({'type':'error','code':e.code,'status':e.status})
        sys.exit(1)
    except Exception as e:
        emit({'type':'error','code':'transport_'+type(e).__name__.lower()})
        sys.exit(1)
