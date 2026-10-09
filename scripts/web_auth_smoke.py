"""Offline account and HTTP adapter authorization regressions."""
import json
import os
import sys
import tempfile
import threading
import urllib.request
import urllib.error
from pathlib import Path
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'src'))
from redteam_agent import AgentService
from redteam_agent.adapters.web import WebApi
from redteam_agent.adapters.web_accounts import Accounts
from redteam_agent.adapters.web_server import TraceHTTPServer


def sse_revocation(api):
    token = api.control.login('trace', 'admin@123')
    entered, release = threading.Event(), threading.Event()
    responses = []
    def delayed_events(*args, **kwargs):
        entered.set()
        assert release.wait(5)
        return [{'sequence': 1, 'event_type': 'after-revoke', 'payload': 'private-event'}]
    server = TraceHTTPServer(('127.0.0.1', 0), api)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    def fetch():
        request = urllib.request.Request(
            f'http://127.0.0.1:{server.server_port}/api/runs/fixture/events?wait_seconds=20',
            headers={'Authorization': 'Bearer ' + token, 'Accept': 'text/event-stream'})
        try:
            response = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            responses.append((response.status, response.read()))
    client = threading.Thread(target=fetch)
    try:
        with patch.object(api, 'sse_events', delayed_events):
            client.start()
            assert entered.wait(5)
            api.control.logout({'authorization': 'Bearer ' + token})
            release.set()
            client.join(5)
            assert not client.is_alive()
        assert responses and responses[0][0] == 401, responses
        assert b'private-event' not in responses[0][1]
    finally:
        release.set()
        server.shutdown()
        server.server_close()
        thread.join(5)


def main():
    with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {'TRACE_ADMIN_USERNAME': 'trace', 'TRACE_ADMIN_PASSWORD': 'admin@123', 'TRACE_AUTH_REQUIRED': '0', 'TRACE_ADMIN_TOKEN': 'old-token'}):
        root = Path(directory)
        with AgentService(root=root, load_external_configuration=False) as service:
            api = WebApi(service)
            sse_revocation(api)
            assert api.dispatch('GET', '/api/system').status == 401
            assert api.dispatch('GET', '/api/auth/status').payload()['required'] is True
            assert api.dispatch('POST', '/api/auth/login', body={'password': 'admin@123'}).status == 401
            assert api.dispatch('POST', '/api/auth/login', body={'token': 'old-token'}).status == 401
            login = api.dispatch('POST', '/api/auth/login', body={'username': 'TRACE', 'password': 'admin@123'})
            assert login.status == 200, login.payload()
            headers = {'cookie': login.headers['Set-Cookie'].split(';')[0]}
            identity = login.payload()['user']['user_id']
            def request(method, route, body=None, auth=headers):
                return api.dispatch(method, route, body=body, headers=auth)
            assert request('POST', '/api/auth/users', {'username': 'member', 'password': 'member-pass'}).status == 200
            assert request('POST', '/api/auth/users', {'username': 'MEMBER', 'password': 'other'}).status == 400
            from concurrent.futures import ThreadPoolExecutor
            def create_duplicate(name):
                return request('POST', '/api/auth/users', {'username': name, 'password': 'race-pass'}).status
            with ThreadPoolExecutor(max_workers=2) as pool:
                assert sorted(pool.map(create_duplicate, ('Concurrent', 'CONCURRENT'))) == [200, 400]
            member = api.control.login('member', 'member-pass')
            mh = {'authorization': 'Bearer ' + member}
            for route in ['/api/providers', '/api/skills/test', '/api/mcp', '/api/system/reload', '/api/auth/users']:
                assert request('POST', route, {}, mh).status == 403, route
            assert request('GET', '/api/system', auth=mh).status == 200
            assert request('POST', '/api/auth/profile', {'role': 'admin'}, mh).payload()['user']['role'] == 'member'
            assert request('POST', '/api/auth/profile', {'username': 'rename-denied'}).status == 400
            assert request('GET', '/api/auth/profile').status == 200
            before_rename = api.control.login('trace', 'admin@123')
            assert request('POST', '/api/auth/profile', {'username': 'trace-owner', 'current_password': 'admin@123'}).status == 200
            assert not api.control.authenticated({'authorization': 'Bearer ' + before_rename})
            assert request('GET', '/api/auth/profile').status == 200
            assert request('POST', '/api/auth/profile', {'display_name': 'Display only'}).status == 200
            second = api.control.login('trace-owner', 'admin@123')
            other = Accounts(api.control.store)
            remote = other.login('trace-owner', 'admin@123')
            assert request('POST', '/api/auth/profile', {'password': 'new-pass'}).status == 400
            changed = request('POST', '/api/auth/profile', {'username': 'renamed', 'display_name': 'Owner', 'password': 'new-pass', 'current_password': 'admin@123'})
            assert changed.status == 200, changed.payload()
            assert changed.payload()['user']['user_id'] == identity
            assert request('GET', '/api/auth/profile').status == 200
            assert not api.control.authenticated({'authorization': 'Bearer ' + second})
            assert other.user({'authorization': 'Bearer ' + remote}) is None
            with api.control.store.transaction() as db:
                row = db.execute('SELECT * FROM trace_users WHERE user_id=?', (identity,)).fetchone()
                assert row['password_hash'] != 'new-pass' and row['salt']
        with AgentService(root=root, load_external_configuration=False) as service:
            api = WebApi(service)
            assert api.dispatch('POST', '/api/auth/login', body={'username': 'trace', 'password': 'admin@123'}).status == 401
            result = api.dispatch('POST', '/api/auth/login', body={'username': 'renamed', 'password': 'new-pass'})
            assert result.status == 200 and result.payload()['user']['user_id'] == identity
    print(json.dumps({'status': 'passed', 'checks': ['mandatory_login', 'no_legacy_bypass', 'admin_authorization', 'casefold_uniqueness', 'profile', 'password_rotation', 'cross_process_session_revision', 'restart_persistence', 'sse_revocation_during_wait']}))

if __name__ == '__main__':
    main()
