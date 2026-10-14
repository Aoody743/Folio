#!/usr/bin/env python3
"""Real packaged OpenResty/FPM migration; requires exclusive disposable VM.

Private fixture (0600 outside checkout): url, username, password, version,
source_web, target_web, php_instance, site_url. Run inside the marked VM as root. The two Web
instances and FPM must already be managed, running, and QA-owned. This script
creates its own unique website, exercises REST and native HTTP MCP, and deletes
only that website/content afterwards. It does not provision host services.
"""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import secrets
import stat
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from http_client import APIClient
from mcp_client import HTTPMCPClient, tool_data


class ActionFailure(AssertionError):
    def __init__(self, name, status, code):
        super().__init__(f'{name}: HTTP {status}, code={code}')
        self.code = code


def action(api, name, body, key=None):
    status, payload, _ = api.post('/api/v1/actions/' + name, body,
                                 headers={'Idempotency-Key': key} if key else None)
    if status != 200 or not isinstance(payload, dict) or 'data' not in payload:
        code = payload.get('error', {}).get('code', 'unknown') if isinstance(payload, dict) else 'unknown'
        raise ActionFailure(name, status, code)
    return payload['data']


def wait(api, task):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        item = action(api, 'tasks_get', {'id': task['id']})
        state = item.get('status', item.get('state'))
        if state in ('succeeded', 'completed'):
            return item
        if state in ('failed', 'canceled', 'cancelled', 'interrupted'):
            if os.geteuid() == 0:
                diagnostic = Path('/root/powerlinux-qa/native-site-task-failure.json')
                diagnostic.write_text(json.dumps(item))
                diagnostic.chmod(0o600)
            raise AssertionError(f'task ended in {state}; private diagnostic retained')
        time.sleep(.4)
    raise AssertionError('task timeout')


def response(base, domain, path):
    request = urllib.request.Request(base.rstrip('/') + path, headers={'Host': domain})
    try:
        stream = urllib.request.urlopen(request, timeout=10)
    except urllib.error.HTTPError as error:
        stream = error
    with stream:
        return stream.status, stream.read(1 << 20)


def main():
    if os.environ.get('POWERLINUX_QA_EXCLUSIVE_WRITE_WINDOW') != 'YES':
        raise RuntimeError('An exclusive disposable QA VM window is required')
    path = Path(os.environ['POWERLINUX_QA_MIGRATION_FILE'])
    if path.is_symlink() or not path.is_file() or stat.S_IMODE(path.stat().st_mode) != 0o600:
        raise RuntimeError('Fixture must be a private regular 0600 file')
    if path.resolve().is_relative_to(Path(__file__).resolve().parents[3]):
        raise RuntimeError('Fixture must be outside checkout')
    marker_path = Path('/run/powerlinux-qa-disposable')
    if os.geteuid() != 0 or marker_path.is_symlink() or marker_path.stat().st_uid != 0 or stat.S_IMODE(marker_path.stat().st_mode) != 0o400 or marker_path.read_text().strip() != 'powerlinux-local-qa-20261014':
        raise RuntimeError('Native ownership checks require the explicitly marked disposable VM')
    fixture = json.loads(path.read_text())
    api = APIClient(fixture['url'])
    status, login, _ = api.post('/api/v1/auth/login', {
        'username': fixture['username'], 'password': fixture['password']}, csrf=False)
    if status != 200:
        raise RuntimeError('QA login failed')
    api.csrf = login['data']['csrf_token']
    status, health, _ = api.get('/healthz')
    assert status == 200 and health['version'] == fixture['version'], 'Wrong package version'
    runtimes = {}
    for name, plugin in [('source_web', 'openresty'), ('target_web', 'openresty'), ('php_instance', 'php-apt')]:
        instance = action(api, 'plugins_get', {'id': fixture[name]})
        runtimes[fixture[name]] = instance
        assert instance['plugin_id'] == plugin and instance['status'] == 'running', 'Wrong native runtime fixture'
    site_id = 'migration-' + secrets.token_hex(6)
    domain = site_id + '.example.test'
    creation_task = None
    token_id = None
    checks = []
    marker = 'PowerLinux-' + secrets.token_hex(12)
    try:
        try:
            action(api, 'sites_get', {'id': site_id})
        except ActionFailure as error:
            if error.code != 'not_found':
                raise
        else:
            raise RuntimeError('Random QA site ID is already in use')
        creation_task = action(api, 'sites_create', {'id': site_id, 'name': site_id,
            'domains': [domain], 'web_instance': fixture['source_web'],
            'php_instance': fixture['php_instance'], 'index': ['index.php', 'index.html']}, site_id + '-create')
        wait(api, creation_task)
        site = action(api, 'sites_get', {'id': site_id})
        root, php = site['document_root'], site['php_instance']
        worker = Path(root).stat().st_uid
        php_pool = Path(runtimes[php]['paths']['config_dir']) / 'pools' / (site_id + '.conf')
        pool_hash = hashlib.sha256(php_pool.read_bytes()).hexdigest()
        body = '<?php header("Content-Type: application/json"); echo json_encode(["marker"=>"' + marker + '","version"=>PHP_VERSION,"sapi"=>PHP_SAPI]);'
        action(api, 'fs_create', {'root': 'site-' + site_id, 'path': 'chain.php', 'content': body, 'encoding': 'utf-8'})
        original = action(api, 'fs_read', {'root': 'site-' + site_id, 'path': 'chain.php', 'encoding': 'utf-8'})
        status, raw = response(fixture['site_url'], domain, '/chain.php')
        data = json.loads(raw)
        assert status == 200 and data['marker'] == marker and data['sapi'] == 'fpm-fcgi', 'Native PHP response mismatch'
        checks.append('actual source OpenResty/FPM response')
        print('Native PHP/FPM response verified', flush=True)
        for target in (fixture['target_web'], fixture['source_web']):
            print('Native migration target: ' + target, flush=True)
            site = action(api, 'sites_get', {'id': site_id})
            plan = action(api, 'sites_migration_plan', {'id': site_id, 'expected_revision': site['revision'],
                'target_web_instance': target, 'reset_advanced_config': False})
            reviewed = action(api, 'sites_migration_get', {'plan_id': plan['id']})
            assert reviewed == plan, 'Plan changed before review'
            values = {'plan_id': plan['id'], 'expected_revision': site['revision']}
            key = site_id + '-' + target
            first = action(api, 'sites_migration_apply', values, key)
            repeated = action(api, 'sites_migration_apply', values, key)
            assert first['id'] == repeated['id'], 'Repeated intent queued another migration'
            wait(api, first)
            after = action(api, 'sites_get', {'id': site_id})
            assert after['web_instance'] == target and after['php_instance'] == php and after['document_root'] == root
            assert action(api, 'fs_read', {'root': 'site-' + site_id, 'path': 'chain.php', 'encoding': 'utf-8'}) == original
            assert Path(root).stat().st_uid == worker, 'Website worker changed'
            assert hashlib.sha256(php_pool.read_bytes()).hexdigest() == pool_hash, 'PHP pool changed'
            old_vhost = Path(runtimes[site['web_instance']]['paths']['config_dir']) / 'sites' / (site_id + '.conf')
            new_vhost = Path(runtimes[target]['paths']['config_dir']) / 'sites' / (site_id + '.conf')
            assert not old_vhost.exists() and new_vhost.is_file(), 'Vhost ownership move mismatch'
            status, raw = response(fixture['site_url'], domain, '/chain.php')
            assert status == 200 and json.loads(raw) == data, 'Migration changed native response'
            checks.append('native migration to ' + target + '; content/FPM/root preserved; old vhost absent; idempotent task')
        status, token, _ = api.post('/api/v1/tokens', {'name': 'QA migration read', 'kind': 'mcp',
            'scopes': ['sites:read'], 'resources': ['site:' + site_id]})
        assert status == 200
        token_id = token['data']['id']
        mcp = HTTPMCPClient(fixture['url'].rstrip('/') + '/mcp', token['data']['token'])
        mcp.initialize()
        viewed = tool_data(mcp.call('sites_get', {'id': site_id}))
        assert viewed['web_instance'] == fixture['source_web'], 'HTTP MCP resource disagrees with REST'
        checks.append('native HTTP MCP scoped site read agrees with REST')
        print(json.dumps({'version': health['version'], 'scope': 'actual installed native OpenResty/PHP-FPM and REST/HTTP MCP',
                          'checks': checks, 'php_version': data['version'], 'content_sha256': hashlib.sha256(body.encode()).hexdigest()}, ensure_ascii=False))
    finally:
        cleanup_errors = []
        if token_id:
            try:
                status, _, _ = api.delete('/api/v1/tokens/' + token_id)
                assert status == 200, 'QA token cleanup failed'
            except Exception as error:
                cleanup_errors.append(type(error).__name__ + ': token cleanup failed')
        if creation_task:
            try:
                job = action(api, 'tasks_get', {'id': creation_task['id']})
                if job.get('status', job.get('state')) not in ('succeeded', 'completed', 'failed', 'canceled', 'cancelled', 'interrupted'):
                    action(api, 'tasks_cancel', {'id': creation_task['id']})
                    try:
                        wait(api, creation_task)
                    except AssertionError:
                        pass
                    job = action(api, 'tasks_get', {'id': creation_task['id']})
                    assert job.get('status', job.get('state')) in ('succeeded', 'completed', 'failed', 'canceled', 'cancelled', 'interrupted'), 'QA creation task still running'
                try:
                    current = action(api, 'sites_get', {'id': site_id})
                except ActionFailure as error:
                    if error.code != 'not_found':
                        raise
                else:
                    wait(api, action(api, 'sites_delete', {'id': site_id, 'expected_revision': current['revision'], 'keep_data': False}, site_id + '-delete'))
            except Exception as error:
                cleanup_errors.append(type(error).__name__ + ': site cleanup failed for ' + site_id)
        if cleanup_errors:
            raise RuntimeError('; '.join(cleanup_errors))


if __name__ == '__main__':
    main()
