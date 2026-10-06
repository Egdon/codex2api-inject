#!/usr/bin/env python3
# Python 3.11+ | Standard library | Offline release-assembly regression
# Run: python3 .github/scripts/test-assemble-patched-binaries.py
# Deps: git, bash, tar, sha256sum; synthetic Go executable, no compilation/network.
import hashlib
import json
import os
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
FAKE_GO = r'''#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
args = sys.argv[1:]
if args[0] == 'build':
    dirty = subprocess.check_output(['git','status','--porcelain','--untracked-files=normal'], text=True).strip()
    assert not dirty, 'assembly dirtied checkout before subsequent platform'
    assert not pathlib.Path('dist').exists(), 'upload staging appeared before final platform'
    output = pathlib.Path(args[args.index('-o')+1])
    assert pathlib.Path.cwd() not in output.resolve().parents
    assert os.environ['CGO_ENABLED']=='0'
    ldflags = next(a for a in args if a.startswith('-ldflags='))
    for value in (os.environ['RELEASE_TAG'], os.environ['CANDIDATE_SHA'], os.environ['UPSTREAM_BASE'], 'Source=patched'):
        assert value in ldflags
    with open(os.environ['CALL_LOG'],'a') as log:
        log.write(json.dumps({'os':os.environ['GOOS'],'arch':os.environ['GOARCH']})+'\n')
    output.write_text(json.dumps({'GOOS':os.environ['GOOS'],'GOARCH':os.environ['GOARCH']}))
    if os.environ.get('GO_MUTATES_CHECKOUT'):
        pathlib.Path('unexpected-source-change').write_text('synthetic')
else:
    assert args[:2] == ['version','-m']
    platform = json.loads(pathlib.Path(args[2]).read_text())
    fields = {'vcs':'git','vcs.revision':os.environ['CANDIDATE_SHA'],'vcs.modified':'false','CGO_ENABLED':'0',**platform}
    bad = os.environ.get('BAD_PROVENANCE')
    if bad == 'dirty': fields['vcs.modified']='true'
    if bad == 'revision': fields['vcs.revision']='0'*40
    if bad == 'arch': fields['GOARCH']='wrong'
    if bad == 'missing': del fields['vcs.modified']
    print(args[2]+': go-test')
    for key,value in fields.items(): print('\tbuild\t'+key+'='+value)
'''


class AssemblyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.repo, self.runner, self.bin = root/'repo', root/'runner', root/'bin'
        for path in (self.repo, self.runner, self.bin): path.mkdir()
        (self.repo/'.github/scripts').mkdir(parents=True)
        for name in ('assemble-patched-binaries.sh','patched-release-common.sh'):
            (self.repo/'.github/scripts'/name).write_bytes((SCRIPTS/name).read_bytes())
        (self.repo/'README.md').write_text('Synthetic release fixture\n')
        (self.repo/'.env.example').write_text('SYNTHETIC_CONFIG=true\n')
        (self.bin/'go').write_text(FAKE_GO)
        (self.bin/'go').chmod(0o755)
        self.git('init','-q')
        self.git('add','.')
        self.git('-c','user.name=Test','-c','user.email=test@example.invalid','commit','-qm','fixture')
        self.sha=self.git('rev-parse','HEAD').strip()
        self.env={**os.environ,'PATH':str(self.bin)+os.pathsep+os.environ['PATH'],
                  'RELEASE_TAG':'patched-v1.1.0','CANDIDATE_SHA':self.sha,'UPSTREAM_BASE':'v3.0.7',
                  'RUNNER_TEMP':str(self.runner),'CALL_LOG':str(root/'calls.jsonl')}

    def git(self,*args):
        return subprocess.check_output(['git','-C',str(self.repo),*args],text=True,stderr=subprocess.PIPE)

    def run_assembly(self,**env):
        return subprocess.run(['bash','.github/scripts/assemble-patched-binaries.sh'],cwd=self.repo,
                              env={**self.env,**env},capture_output=True,text=True,timeout=30)

    def assert_no_assets(self,result):
        self.assertNotEqual(result.returncode,0)
        self.assertFalse((self.repo/'dist').exists())
        self.assertEqual(list(self.runner.iterdir()),[])

    def test_four_platforms_packaged_after_all_clean_builds(self):
        result=self.run_assembly()
        self.assertEqual(result.returncode,0,result.stderr)
        calls=[json.loads(line) for line in Path(self.env['CALL_LOG']).read_text().splitlines()]
        self.assertEqual(calls,[{'os':o,'arch':a} for o in ('linux','darwin') for a in ('amd64','arm64')])
        assets=list((self.repo/'dist').iterdir())
        self.assertEqual(len(assets),5)
        for line in (self.repo/'dist/SHA256SUMS.txt').read_text().splitlines():
            digest,name=line.split('  ')
            path=self.repo/'dist'/name
            self.assertEqual(hashlib.sha256(path.read_bytes()).hexdigest(),digest)
            with tarfile.open(path) as archive:
                self.assertEqual(set(archive.getnames()),{'codex2api','.env.example','README.md'})
        self.assertEqual(list(self.runner.iterdir()),[])
        self.assertEqual(self.git('diff','--name-only'),'')

    def test_dirty_checkout_rejected_before_compiling(self):
        (self.repo/'README.md').write_text('changed')
        self.assert_no_assets(self.run_assembly())
        self.assertFalse(Path(self.env['CALL_LOG']).exists())

    def test_untracked_checkout_rejected_before_compiling(self):
        (self.repo/'untracked').write_text('data')
        self.assert_no_assets(self.run_assembly())

    def test_dirty_or_incorrect_build_metadata_fails_closed(self):
        for failure in ('dirty','revision','arch','missing'):
            with self.subTest(failure=failure): self.assert_no_assets(self.run_assembly(BAD_PROVENANCE=failure))

    def test_build_modification_prevents_next_target_and_staging(self):
        self.assert_no_assets(self.run_assembly(GO_MUTATES_CHECKOUT='yes'))
        self.assertEqual(len(Path(self.env['CALL_LOG']).read_text().splitlines()),1)

    def test_runner_inside_checkout_is_rejected(self):
        self.assert_no_assets(self.run_assembly(RUNNER_TEMP=str(self.repo)))

    def test_existing_dist_is_not_overwritten(self):
        (self.repo/'dist').mkdir()
        marker=self.repo/'dist/existing';marker.write_text('keep')
        self.assertNotEqual(self.run_assembly().returncode,0)
        self.assertEqual(marker.read_text(),'keep')
        self.assertFalse(Path(self.env['CALL_LOG']).exists())

    def test_noncanonical_tag_or_revision_rejected(self):
        for patch in ({'RELEASE_TAG':'v1.1.0'},{'RELEASE_TAG':'patched-v01.1.0'},{'CANDIDATE_SHA':'f'*40}):
            with self.subTest(patch=patch): self.assert_no_assets(self.run_assembly(**patch))


if __name__=='__main__':
    unittest.main()
