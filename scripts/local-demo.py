#!/usr/bin/env python3
"""Isolated localhost demo. Requires Docker, Python 3 and a built frontend."""
import base64
import json
import os
from pathlib import Path
import platform
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
import webbrowser
import plistlib

ROOT = Path(__file__).resolve().parent.parent
PRIVATE = ROOT / '.secrets/local-demo.json'
OUTPUT = ROOT / 'artifacts/local-demo'
CONTAINER = 'gptibo-local-demo-db'
GO_IMAGE = 'golang:1.27.1-bookworm'


def run(args, **kwargs):
    return subprocess.run(args, check=True, cwd=ROOT, **kwargs)


def config():
    if not PRIVATE.exists():
        # Check host port before registering a new persistent service.
        for port in (5446, 8788):
            with socket.socket() as sock:
                if sock.connect_ex(('127.0.0.1', port)) == 0:
                    raise SystemExit(f'Port {port} is occupied; choose a free demo port before setup.')
        PRIVATE.parent.mkdir(parents=True, exist_ok=True)
        c = dict(database='gptibo_demo', ownerPassword=secrets.token_hex(32),
                 runtimePassword=secrets.token_hex(32), secret=secrets.token_hex(32),
                 totp=base64.b32encode(secrets.token_bytes(20)).decode(),
                 launchKey=secrets.token_hex(32), origin='http://localhost:8788',
                 listen='127.0.0.1:8788', dist=str(ROOT / 'dist'),
                 state=str(ROOT / '.secrets/local-demo-state.json'), dbPort='5446')
        fd = os.open(PRIVATE, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'w') as file:
            json.dump(c, file, indent=2)
    if PRIVATE.stat().st_mode & 0o077:
        raise SystemExit('Private demo config must have mode 600.')
    return json.loads(PRIVATE.read_text())


def database(c):
    names = subprocess.check_output(['docker', 'ps', '-a', '--format', '{{.Names}}'], text=True).splitlines()
    if CONTAINER not in names:
        env = os.environ.copy()
        env['POSTGRES_PASSWORD'] = c['ownerPassword']
        run(['docker', 'run', '-d', '--name', CONTAINER, '--restart', 'unless-stopped',
             '-e', 'POSTGRES_PASSWORD', '-e', 'POSTGRES_DB=gptibo_demo',
             '-p', '127.0.0.1:' + c.get('dbPort', '5446') + ':5432',
             '-v', 'gptibo-local-demo-pg:/var/lib/postgresql/data', 'postgres:16-alpine'], env=env,
            stdout=subprocess.DEVNULL)
    else:
        run(['docker', 'start', CONTAINER], stdout=subprocess.DEVNULL)
    for _ in range(30):
        if subprocess.run(['docker', 'exec', CONTAINER, 'pg_isready', '-U', 'postgres'],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0:
            return
        time.sleep(1)
    raise SystemExit('Demo database did not become ready.')


def build():
    OUTPUT.mkdir(parents=True, exist_ok=True)
    arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}[platform.machine()]
    target = {'Darwin': 'darwin', 'Linux': 'linux'}[platform.system()]
    run(['docker', 'run', '--rm', '-v', str(ROOT) + ':/repo',
         '-v', 'gptibo-go-mod:/go/pkg/mod', '-v', 'gptibo-go-build:/root/.cache/go-build',
         '-w', '/repo/services/storefront', GO_IMAGE, 'env', 'CGO_ENABLED=0',
         'GOOS=' + target, 'GOARCH=' + arch, 'go', 'build',
         '-o', '/repo/artifacts/local-demo/localdemo-next', './cmd/localdemo'])
    (OUTPUT / 'localdemo-next').replace(OUTPUT / 'localdemo')


def install(c):
    if platform.system() != 'Darwin':
        raise SystemExit('install is for macOS launchd; use start on other platforms.')
    database(c)
    if not (OUTPUT / 'localdemo').exists():
        build()
    if not (ROOT / 'dist/index.html').exists():
        raise SystemExit('Build the frontend first: npm run build')
    # Keep the complete runtime on the internal disk. New background executables
    # cannot assume Terminal's removable-volume access permissions on macOS.
    app = Path.home() / 'Library/Application Support/GPTibo'
    app.mkdir(parents=True, exist_ok=True)
    app.chmod(0o700)
    runtime = dict(c, dist=str(app / 'dist'), state=str(app / 'local-demo-state.json'))
    if not Path(runtime['state']).exists() and Path(c['state']).exists():
        shutil.copy2(c['state'], runtime['state'])
        Path(runtime['state']).chmod(0o600)
    runtime_config = app / 'local-demo.json'
    fd = os.open(runtime_config, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'w') as file:
        json.dump(runtime, file, indent=2)
    runtime_config.chmod(0o600)
    shutil.copytree(ROOT / 'dist', app / 'dist', dirs_exist_ok=True)
    shutil.copy2(OUTPUT / 'localdemo', app / 'localdemo-next')
    (app / 'localdemo-next').replace(app / 'localdemo')
    agents = Path.home() / 'Library/LaunchAgents'
    agents.mkdir(parents=True, exist_ok=True)
    path = agents / 'local.gptibo.plist'
    backup = agents / 'local.gptibo.worker.plist.backup'
    if path.exists() and not backup.exists():
        shutil.copy2(path, backup)
    domain = 'gui/' + str(os.getuid())
    subprocess.run(['launchctl', 'bootout', domain, str(path)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    logs = Path.home() / 'Library/Logs/gptibo'
    logs.mkdir(parents=True, exist_ok=True)
    settings = dict(Label='local.gptibo', ProgramArguments=[str(app / 'localdemo'), str(runtime_config)],
                    WorkingDirectory=str(app), RunAtLoad=True, KeepAlive=True, ThrottleInterval=15,
                    StandardOutPath=str(logs / 'stdout.log'), StandardErrorPath=str(logs / 'stderr.log'))
    path.write_bytes(plistlib.dumps(settings))
    run(['launchctl', 'bootstrap', domain, str(path)])
    for _ in range(30):
        try:
            with urllib.request.urlopen(c['origin'] + '/healthz', timeout=2) as response:
                if json.load(response).get('ready'):
                    print('Installed and verified persistent local demo:', c['origin'],
                          '(database on loopback port ' + c.get('dbPort', '5446') + ')')
                    return
        except (OSError, ValueError):
            pass
        time.sleep(1)
    raise SystemExit('Demo installed but health check did not pass; inspect ~/Library/Logs/gptibo.')


def main():
    command = sys.argv[1] if len(sys.argv) > 1 else 'start'
    c = config()
    if command == 'build':
        build()
    elif command == 'install':
        install(c)
    elif command in ('setup', 'start'):
        database(c)
        if command == 'setup' or not (OUTPUT / 'localdemo').exists():
            build()
        if command == 'start':
            if not (ROOT / 'dist/index.html').exists():
                raise SystemExit('Build the frontend first: npm run build')
            os.execv(str(OUTPUT / 'localdemo'), [str(OUTPUT / 'localdemo'), str(PRIVATE)])
        print('Demo prepared. Start with: python3 scripts/local-demo.py start')
    elif command == 'open':
        webbrowser.open(c['origin'] + '/local-demo/start?key=' + c['launchKey'])
        print('Opened the private local demo entry.')
    elif command == 'status':
        with urllib.request.urlopen(c['origin'] + '/healthz', timeout=5) as response:
            print(json.dumps(json.load(response), ensure_ascii=False))
    else:
        raise SystemExit('Usage: local-demo.py setup|build|start|install|open|status')


if __name__ == '__main__':
    main()
