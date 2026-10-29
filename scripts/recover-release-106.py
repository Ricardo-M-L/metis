#!/usr/bin/env python3
"""One-use v106 recovery guard. Never dispatches, cancels, or publishes a run.

The reviewed tooling branch must put this file and its expectations in scripts/.
Every command requires the original attempt to have ended exclusively because
the ARM Mac runner was never acquired. A queued run is always rejected.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import zipfile

sys.dont_write_bytecode = True
EXPECTED = json.loads(Path(__file__).with_name('recover-release-106-expected.json').read_text())
REPO = EXPECTED['repository']
TAG = EXPECTED['tag']
SHA = EXPECTED['source_sha']
RUN = EXPECTED['original_run_id']
CAPACITY_MESSAGES = {
    'The job was not acquired by Runner of type hosted even after multiple attempts',
    'Due to capacity constraints, jobs targeting macOS arm64 runners may experience longer queue times.',
}
ALLOWED_TOOLING_FILES = {
    '.github/workflows/release.yml', '.github/workflows/release-published.yml',
    'scripts/recover-release-106.py', 'scripts/recover-release-106-expected.json',
}
MAC_NAMES = {f'metis-desktop-darwin-universal.{suffix}'
             for suffix in ('zip', 'zip.sha256', 'dmg', 'dmg.sha256')}


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def run(*args, **kwargs):
    return subprocess.check_output([str(x) for x in args], text=True, **kwargs)


def api(path):
    return json.loads(run('gh', 'api', f'repos/{REPO}/{path}'))


def streamhash(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def release_identity(metadata):
    # Download counters change during our own verification; asset IDs/bytes must not.
    return (metadata['id'], metadata['tag_name'], metadata['draft'], metadata['prerelease'],
            sorted((a['id'], a['name'], a['size'], a['state'], a.get('updated_at'), a.get('digest'))
                   for a in metadata['assets']))


def validate_original(record, listing, artifacts, check, annotations):
    require(record.get('id') == RUN and record.get('run_attempt') == EXPECTED['original_run_attempt'],
            'original run/attempt changed')
    require(record.get('head_sha') == SHA and record.get('head_branch') == TAG
            and record.get('event') == 'push' and record.get('path') == EXPECTED['workflow_path']
            and record.get('workflow_id') == EXPECTED['workflow_id'], 'original source/workflow mismatch')
    for key in ('repository', 'head_repository'):
        require(record.get(key, {}).get('id') == EXPECTED['repository_id']
                and record.get(key, {}).get('full_name') == REPO, 'original repository mismatch')
    require(record.get('status') == 'completed' and record.get('conclusion') in ('failure', 'cancelled'),
            'original run must already be terminal infrastructure failure; queued/success is forbidden')
    jobs = listing.get('jobs', [])
    require(listing.get('total_count') == len(jobs) == 8, 'incomplete/unexpected original job inventory')
    by_name = {j['name']: j for j in jobs}
    require(len(by_name) == 8 and set(by_name) == set(EXPECTED['success_jobs']) |
            {'Desktop darwin-universal', 'Stage immutable release draft'}, 'original job identities changed')
    for name, ident in EXPECTED['success_jobs'].items():
        job = by_name[name]
        require(job.get('id') == ident and job.get('run_attempt') == EXPECTED['original_run_attempt']
                and job.get('run_id') == RUN and job.get('head_sha') == SHA
                and job.get('status') == 'completed' and job.get('conclusion') == 'success',
                'original required gate did not pass: ' + name)
    mac = by_name['Desktop darwin-universal']
    require(mac.get('id') == EXPECTED['mac_job_id'] and mac.get('run_id') == RUN
            and mac.get('run_attempt') == EXPECTED['original_run_attempt']
            and mac.get('head_sha') == SHA and mac.get('status') == 'completed'
            and mac.get('conclusion') == 'cancelled' and mac.get('steps') == []
            and mac.get('runner_id') == 0 and not mac.get('runner_name'),
            'Mac failure was not the fixed zero-step capacity cancellation')
    stage = by_name['Stage immutable release draft']
    require(stage.get('run_id') == RUN and stage.get('head_sha') == SHA
            and stage.get('run_attempt') == EXPECTED['original_run_attempt']
            and stage.get('status') == 'completed' and stage.get('conclusion') == 'skipped'
            and stage.get('steps') == [], 'original stage executed or did not skip')
    require(check.get('id') == EXPECTED['mac_job_id'] and check.get('head_sha') == SHA
            and check.get('status') == 'completed' and check.get('conclusion') == 'cancelled'
            and check.get('output', {}).get('annotations_count') == len(annotations),
            'Mac check identity/annotation completeness mismatch')
    require(len(annotations) >= 2 and {a.get('message') for a in annotations} == CAPACITY_MESSAGES,
            'Mac failure lacks the exact observed runner-capacity diagnostics')
    for annotation in annotations:
        level = 'failure' if annotation['message'].startswith('The job') else 'notice'
        require(annotation.get('annotation_level') == level and annotation.get('path') == '.github',
                'unexpected Mac annotation severity/path')
    entries = artifacts.get('artifacts', [])
    require(artifacts.get('total_count') == len(entries) == 4, 'original artifact inventory changed')
    fixed = {a['id']: a for a in EXPECTED['artifacts']}
    require({a.get('id') for a in entries} == set(fixed), 'original artifact IDs changed')
    for artifact in entries:
        require(artifact.get('expired') is False, 'original artifact expired')
        require(all(artifact.get(k) == value for k, value in fixed[artifact['id']].items()),
                'original artifact metadata/digest changed')


def check_original(evidence):
    record = api(f'actions/runs/{RUN}')
    jobs = api(f'actions/runs/{RUN}/jobs?filter=latest&per_page=100')
    artifacts = api(f'actions/runs/{RUN}/artifacts?per_page=100')
    check = api(f"check-runs/{EXPECTED['mac_job_id']}")
    annotations = api(f"check-runs/{EXPECTED['mac_job_id']}/annotations?per_page=100")
    validate_original(record, jobs, artifacts, check, annotations)
    for name, data in [('original-run', record), ('original-jobs', jobs), ('original-artifacts', artifacts),
                       ('mac-check', check), ('mac-annotations', annotations)]:
        (evidence / (name + '.json')).write_text(json.dumps(data, indent=2) + '\n')


def check_context(args, evidence):
    require(re.fullmatch('[0-9a-f]{40}', args.tooling_sha or '') is not None, 'tooling SHA must be explicit')
    require(args.tag == TAG and os.environ.get('GITHUB_REPOSITORY') == REPO
            and os.environ.get('GITHUB_EVENT_NAME') == 'workflow_dispatch'
            and os.environ.get('GITHUB_SHA') == args.tooling_sha, 'unexpected recovery dispatch identity')
    require(run('git', '-C', args.source, 'rev-parse', 'HEAD').strip() == SHA, 'source checkout changed')
    require(run('git', '-C', Path(__file__).resolve().parents[1], 'rev-parse', 'HEAD').strip()
            == args.tooling_sha, 'tooling checkout differs from dispatch')
    ref = api(f'git/ref/tags/{TAG}')['object']
    while ref.get('type') == 'tag':
        ref = api('git/tags/' + ref['sha'])['object']
    require(ref.get('type') == 'commit' and ref.get('sha') == SHA, 'remote tag moved')
    comparison = api(f'compare/{SHA}...{args.tooling_sha}')
    require(comparison.get('merge_base_commit', {}).get('sha') == SHA
            and comparison.get('status') == 'ahead', 'tooling is not descended from exact source')
    files = comparison.get('files', [])
    require(len(files) == 4 and {f.get('filename') for f in files} == ALLOWED_TOOLING_FILES
            and all(f.get('status') in ('added', 'modified') for f in files),
            'tooling branch changes files outside the four reviewed recovery files')
    current = api('actions/runs/' + os.environ['GITHUB_RUN_ID'])
    require(current.get('id') != RUN and current.get('head_sha') == args.tooling_sha
            and current.get('event') == 'workflow_dispatch'
            and current.get('path') == EXPECTED['workflow_path']
            and current.get('workflow_id') == EXPECTED['workflow_id']
            and current.get('repository', {}).get('id') == EXPECTED['repository_id']
            and current.get('head_repository', {}).get('id') == EXPECTED['repository_id'],
            'recovery run identity differs from reviewed tooling')
    (evidence / 'tooling-comparison.json').write_text(json.dumps(comparison, indent=2) + '\n')
    (evidence / 'recovery-run.json').write_text(json.dumps(current, indent=2) + '\n')
    check_original(evidence)


def contract(source):
    spec = importlib.util.spec_from_file_location('original_release_contract', source / 'scripts/release_contract.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def expected_names(source):
    plan = json.loads(run('python3', source / 'scripts/release_contract.py', 'plan', '--tag', TAG))
    require(plan['channel'] == 'stable' and len(plan['assets']) == 24, 'not the full stable release contract')
    return set(plan['assets']) - MAC_NAMES


def artifact_files(name, expected):
    if name.startswith('metis-computer-use-'):
        return {'releases.json'} | {n for n in expected if n.startswith('metis-cu-')}
    if name.startswith('metis-desktop-linux-'):
        return {n for n in expected if n.startswith('metis-desktop-linux-')}
    if name.startswith('metis-desktop-windows-'):
        return {n for n in expected if n.startswith('metis-desktop-windows-')}
    return {n for n in expected if not n.startswith(('metis-cu-', 'metis-desktop-'))}


def download_artifact(artifact, destination, names, evidence):
    archive = evidence / (str(artifact['id']) + '.zip')
    with archive.open('xb') as output:
        subprocess.run(['gh', 'api', f"repos/{REPO}/actions/artifacts/{artifact['id']}/zip"],
                       stdout=output, check=True)
    require(archive.stat().st_size == artifact['size_in_bytes']
            and 'sha256:' + streamhash(archive) == artifact['digest'], 'downloaded artifact digest/size mismatch')
    destination.mkdir(parents=True, exist_ok=False)
    with zipfile.ZipFile(archive) as zipped:
        entries = zipped.infolist()
        require(len(entries) == len(names) and {e.filename for e in entries} == names,
                'artifact payload inventory differs from expected names')
        for entry in entries:
            mode = entry.external_attr >> 16
            require('/' not in entry.filename and '\\' not in entry.filename
                    and not entry.is_dir() and not stat.S_ISLNK(mode)
                    and (stat.S_IFMT(mode) in (0, stat.S_IFREG))
                    and 0 < entry.file_size < 1024 * 1024 * 1024,
                    'artifact contains a non-flat, linked, or invalid file')
            with zipped.open(entry) as source, (destination / entry.filename).open('xb') as output:
                shutil.copyfileobj(source, output, 1024 * 1024)


def download(args, evidence):
    expected = expected_names(args.source)
    args.dist.mkdir(parents=True, exist_ok=False)
    for artifact in EXPECTED['artifacts']:
        is_cu = artifact['name'].startswith('metis-computer-use-')
        if args.scope == 'cu' and not is_cu:
            continue
        names = artifact_files(artifact['name'], expected)
        extracted = evidence / ('artifact-' + str(artifact['id']))
        download_artifact(artifact, extracted, names, evidence)
        for name in names:
            if name == 'releases.json' and args.scope == 'all':
                shutil.copyfile(extracted / name, evidence / 'original-cu-releases.json')
            else:
                with (extracted / name).open('rb') as source, (args.dist / name).open('xb') as output:
                    shutil.copyfileobj(source, output, 1024 * 1024)
    if args.scope == 'all':
        contract(args.source).verify_dist(args.dist, expected)
    else:
        require({p.name for p in args.dist.iterdir()} == artifact_files('metis-computer-use-', expected),
                'CU artifact inventory changed')
    hashes = {p.name: streamhash(p) for p in args.dist.iterdir()}
    (evidence / 'downloaded-file-sha256.json').write_text(json.dumps(hashes, indent=2) + '\n')


def verify_stage(args, evidence):
    expected = expected_names(args.source)
    contract(args.source).verify_dist(args.dist, expected)
    saved = json.loads((args.download_evidence / 'downloaded-file-sha256.json').read_text())
    require(set(saved) == expected and saved == {n: streamhash(args.dist / n) for n in expected},
            'staging files differ from fixed artifact download')
    metadata = json.loads(run('python3', args.source / 'scripts/release_contract.py', 'lookup',
                              '--tag', TAG, '--repository', REPO))
    latest = api('releases/latest')
    registry = json.loads((args.source / '.github/cli-only-releases.json').read_text())
    contract(args.source).verify_release(registry, TAG, metadata, latest, phase='draft', complete=False)
    require({a['name'] for a in metadata['assets']} == expected and len(metadata['assets']) == 20,
            'recovery stage must contain exactly the original 20 non-Mac assets')
    downloaded = evidence / 'remote-stage'
    downloaded.mkdir()
    run('gh', 'release', 'download', TAG, '--repo', REPO, '--dir', downloaded)
    contract(args.source).verify_dist(downloaded, expected)
    require(all(streamhash(downloaded / n) == saved[n] for n in expected),
            'staged release bytes differ from original immutable artifacts')
    after = api('releases/' + str(metadata['id']))
    require(release_identity(after) == release_identity(metadata),
            'draft identity/assets changed during recovery verification')
    check_original(evidence)
    (evidence / 'staged-release.json').write_text(json.dumps(metadata, indent=2) + '\n')
    (evidence / 'recovery-stage-result.json').write_text(json.dumps({
        'tag': TAG, 'dispatch_tag': args.tag, 'tooling_ref': os.environ['GITHUB_REF'],
        'source_sha': SHA, 'tooling_sha': args.tooling_sha, 'original_run': RUN,
        'recovery_run': int(os.environ['GITHUB_RUN_ID']), 'asset_sha256': saved,
        'published': False, 'stage_verified': True}, indent=2) + '\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('check', 'download', 'verify-stage'))
    parser.add_argument('--tag', required=True)
    parser.add_argument('--tooling-sha', required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--scope', choices=('cu', 'all'), default='all')
    parser.add_argument('--dist', type=Path)
    parser.add_argument('--download-evidence', type=Path)
    args = parser.parse_args()
    args.source = args.source.resolve(strict=True)
    args.evidence.mkdir(parents=True, exist_ok=False)
    check_context(args, args.evidence)
    if args.mode == 'download':
        require(args.dist is not None, '--dist is required')
        download(args, args.evidence)
    elif args.mode == 'verify-stage':
        require(args.dist is not None and args.download_evidence is not None,
                '--dist and --download-evidence are required')
        verify_stage(args, args.evidence)
    print('Recovery guard passed: ' + args.mode + '; exact source=' + SHA)


if __name__ == '__main__':
    main()
