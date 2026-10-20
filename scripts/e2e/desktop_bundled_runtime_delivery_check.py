#!/usr/bin/env python3
"""Build and exercise a self-contained local Desktop without installing it.

Every executable is built in an isolated source mirror. Failed-round binaries
are destroyed before returning. A successful round retains only a local ZIP,
checksums and evidence; no CLI or application is installed on the host.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def stamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def interrupted(signum: int, _frame: object) -> None:
    raise InterruptedError(f"delivery interrupted by signal {signum}")


def save(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    temporary.chmod(0o600)
    temporary.replace(path)


def owned_processes(root: Path) -> list[tuple[int, int]]:
    rows = subprocess.check_output(["ps", "-axo", "pid=,pgid=,command="],text=True).splitlines()
    result=[]
    for row in rows:
        fields=row.strip().split(None,2)
        if len(fields)==3 and fields[0].isdigit() and fields[1].isdigit() and str(root)+"/" in fields[2]:
            pid, group=int(fields[0]),int(fields[1])
            if pid!=os.getpid() and group!=os.getpgrp(): result.append((pid,group))
    return result


def stop_owned_processes(root: Path) -> list[int]:
    original=owned_processes(root)
    for group in {group for _,group in original}:
        try:os.killpg(group,signal.SIGTERM)
        except ProcessLookupError:pass
    deadline=time.monotonic()+5
    while owned_processes(root) and time.monotonic()<deadline:time.sleep(.1)
    remaining=owned_processes(root)
    for group in {group for _,group in remaining}:
        try:os.killpg(group,signal.SIGKILL)
        except ProcessLookupError:pass
    deadline=time.monotonic()+5
    while owned_processes(root) and time.monotonic()<deadline:time.sleep(.1)
    return [pid for pid,_ in owned_processes(root)]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", type=Path, default=Path("/Users/yujun2/.local/go-1.26.4/bin/go"))
    parser.add_argument("--wails", type=Path, default=Path("/Users/yujun2/go/bin/wails"))
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("this native acceptance runs on macOS")
    repo, output = args.repo.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    root = Path(tempfile.mkdtemp(prefix="mdb-", dir="/tmp")).resolve()
    root.chmod(0o700)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    source = root / "source"
    report: dict = {"passed": False, "started_utc": stamp(), "checks": {},
        "source_snapshot": str(source), "test_root": str(root),
        "installed_binaries_before": {}, "local_notarized_release": False}
    for installed in (Path.home()/".local/bin/metis",
                      Path("/Applications/METIS.app/Contents/MacOS/metis-desktop"),
                      Path("/Applications/METIS.app/Contents/Resources/bin/metis")):
        if installed.is_file():
            report["installed_binaries_before"][str(installed)] = sha(installed)
    environment = dict(os.environ)
    environment.update({"GOWORK": "off", "PATH": str(args.go.parent)+os.pathsep+environment.get("PATH", "")})

    def progress(phase: str) -> None:
        save(output/"driver-state.json", {**report, "phase": phase, "updated_utc": stamp()})
        print(json.dumps({"phase": phase, "utc": stamp()}), flush=True)

    def run(command: list[str], label: str, cwd: Path | None = None, timeout: int = 1800,
            extra_env: dict[str,str] | None = None) -> None:
        progress(label)
        started = time.monotonic()
        log = output/(label+".log")
        with log.open("wb") as target:
            os.fchmod(target.fileno(), 0o600)
            child = subprocess.Popen(command, cwd=cwd or source,
                env={**environment, **(extra_env or {})}, stdout=target,
                stderr=subprocess.STDOUT, start_new_session=True, umask=0o022)
            try:
                child.wait(timeout=timeout)
            except BaseException:
                if child.poll() is None:
                    child.send_signal(signal.SIGTERM)
                    try:
                        child.wait(timeout=45)
                    except subprocess.TimeoutExpired:
                        os.killpg(child.pid, signal.SIGKILL)
                        child.wait(timeout=10)
                raise
        result = {"exit_code": child.returncode,
            "elapsed_seconds": round(time.monotonic()-started,3), "log": log.name}
        report["checks"][label] = result
        save(output/(label+"-summary.json"),result)
        if child.returncode:
            raise RuntimeError(label+" failed; inspect retained log")

    files: dict[str,str] = {}
    archive: Path | None = None
    try:
        progress("source-freeze")
        # Reuse the repository's isolated-copy policy, including a private
        # dependency copy so Wails/Vite cannot mutate the original checkout.
        spec = importlib.util.spec_from_file_location("desktop_packaging_copy", repo/"scripts/package-local-runtime.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        names = subprocess.check_output(["git","ls-files","-co","--exclude-standard","-z"],cwd=repo).decode().split("\0")
        for name in sorted(set(names)):
            relative=Path(name)
            if not name or any(part in (".git","node_modules","dist","__pycache__", ".private", ".credentials") for part in relative.parts) or relative.suffix==".pyc":
                continue
            original=repo/relative
            if original.is_file() and not original.is_symlink():
                files[name]=sha(original)
        copy_commands=module.Commands()
        report["source_mirror"]=module.copy_build_source(copy_commands,repo,source,[output])
        # Generated build products are deliberately absent from the mirror.
        files={name:digest for name,digest in files.items() if (source/name).is_file()}
        changed=[name for name,digest in files.items() if sha(source/name)!=digest or sha(repo/name)!=digest]
        if changed: raise RuntimeError("source changed during mirror: "+", ".join(changed[:8]))
        revision=subprocess.check_output(["git","rev-parse","HEAD"],cwd=repo,text=True).strip()
        manifest={"created_at_utc":stamp(),"original_repository":str(repo),"snapshot_repository":str(source),
            "captured_revision":revision,"source_files":len(files),"files":files}
        freeze=output/"source-freeze.json"
        save(freeze,manifest)
        report["source_files"]=len(files)
        run([str(args.go),"test","-p","4","-count=1","-timeout=300s","./internal/desktop","./internal/update","./cmd/metis"],"root-go-test")
        run([str(args.go),"vet","./internal/desktop","./internal/update","./cmd/metis"],"root-go-vet")
        run([str(args.go),"test","-race","-count=1","./..."],"native-go-test",cwd=source/"metis-desktop")
        run([str(args.go),"vet","./..."],"native-go-vet",cwd=source/"metis-desktop")
        run([sys.executable,"-B","-m","unittest","discover","-s","scripts","-p","test_prepare_desktop_cli.py"],"packaging-python-test")
        run([sys.executable,"scripts/e2e/desktop_bundled_runtime_acceptance.py","--self-test"],"live-harness-self-test")
        run([sys.executable,"-B","-m","unittest","discover","-s","scripts/e2e","-p","test_desktop_bundled_runtime_delivery_check.py"],"delivery-harness-self-test")
        run(["npm","run","check"],"frontend-check",cwd=source/"metis-desktop/frontend")
        run(["npm","run","build"],"frontend-build",cwd=source/"metis-desktop/frontend")
        built_at=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        for target in ("linux/amd64","windows/amd64"):
            label=target.replace("/","-")
            run([sys.executable,"scripts/prepare-desktop-cli.py","--repo",str(source),
                 "--platform",target,"--go",str(args.go),"--commit",revision[:7]+"-dirty",
                 "--date",built_at,"--source-manifest-sha256",sha(freeze)],"prepare-"+label+"-payload")
            if target=="windows/amd64":
                cross=root/"cross-windows"
                cross.mkdir(mode=0o700)
                run([str(args.go),"build","-trimpath","-tags","production","-o",str(cross/"metis-desktop.exe"),"."],
                    "cross-compile-windows-desktop",cwd=source/"metis-desktop",
                    extra_env={"GOOS":"windows","GOARCH":"amd64","CGO_ENABLED":"0"})
            shutil.rmtree(source/"metis-desktop/embedded-runtime"/label)
        report["cross_platform_boundary"]="Linux/Windows CLI payloads and Windows Desktop cross-compiled; their native execution requires platform CI."
        run([str(args.wails),"build","-s","-skipbindings","-m","-nosyncgomod","-trimpath",
             "-platform","darwin/arm64","-compiler",str(args.go),"-nocolour"],
            "native-build",cwd=source/"metis-desktop",extra_env={"GOOS":"darwin","GOARCH":"arm64","CGO_ENABLED":"1"})
        app=source/"metis-desktop/build/bin/METIS.app"
        if not app.is_dir():raise RuntimeError("native build did not produce METIS.app")
        changed=[name for name,digest in files.items() if not (source/name).is_file() or sha(source/name)!=digest]
        if changed:raise RuntimeError("build modified frozen source: "+", ".join(changed[:8]))
        run([sys.executable,"scripts/prepare-desktop-cli.py","--repo",str(source),"--app",str(app),
             "--platform","darwin/arm64","--go",str(args.go),"--commit",revision[:7]+"-dirty",
             "--date",built_at,"--source-manifest-sha256",sha(freeze)],"bundle-fresh-cli")
        # The inner runtime has already been signed and hashed by the bundler.
        # Signing only the outer app preserves that exact nested executable.
        run(["/usr/bin/codesign","--force","--sign","-",str(app)],"sign-local-app")
        run(["/usr/bin/codesign","--verify","--deep","--strict",str(app)],"verify-local-signature")
        run([sys.executable,"scripts/prepare-desktop-cli.py","--repo",str(source),"--app",str(app),
             "--platform","darwin/arm64","--go",str(args.go),"--verify-only","--execute-version"],
            "verify-bundled-runtime")
        runtime=json.loads((app/"Contents/Resources/bin/metis-runtime.json").read_text())
        cli=app/"Contents/Resources/bin/metis"
        if sha(cli)!=runtime["sha256"]:raise RuntimeError("outer app signing changed the inner runtime")
        report["build"]={"native_sha256":sha(app/"Contents/MacOS/metis-desktop"),"runtime":runtime,
            "source_manifest_sha256":sha(freeze),"native_platform":"darwin/arm64","apple_notarized":False}
        save(output/"build-manifest.json",report["build"])
        candidate=root/"candidate/METIS.app"
        candidate.parent.mkdir(mode=0o700)
        run(["/usr/bin/ditto",str(app),str(candidate)],"copy-local-candidate")
        test_app=root/"Test install with spaces/METIS.app"
        test_app.parent.mkdir(mode=0o700)
        run(["/usr/bin/ditto",str(candidate),str(test_app)],"copy-test-install")
        run([sys.executable,"scripts/e2e/desktop_bundled_runtime_acceptance.py",
             "--app",str(test_app),"--repo",str(source),"--expected-cli-sha256",runtime["sha256"],
             "--source-freeze",str(freeze),"--output",str(output/"live-desktop"),"--destroy-input"],
             "native-real-query",timeout=1200)
        live=json.loads((output/"live-desktop/verification-report.json").read_text())
        if not live.get("passed") or not live.get("cleanup",{}).get("private_CLI_destroyed"):
            raise RuntimeError("native live acceptance/cleanup did not pass")
        changed=[name for name,digest in files.items() if not (repo/name).is_file() or sha(repo/name)!=digest]
        if changed:raise RuntimeError("original source changed while executables existed: "+", ".join(changed[:8]))
        if sha(candidate/"Contents/Resources/bin/metis")!=runtime["sha256"]:
            raise RuntimeError("candidate differs from the tested runtime")
        run(["/usr/bin/codesign","--verify","--deep","--strict",str(candidate)],"verify-tested-candidate-signature")
        archive=output/"METIS-Desktop-local-darwin-arm64.zip"
        run(["/usr/bin/ditto","-c","-k","--sequesterRsrc","--keepParent",str(candidate),str(archive)],"archive-desktop")
        run(["/usr/bin/unzip","-tq",str(archive)],"verify-archive")
        roundtrip=root/"Archive roundtrip"
        roundtrip.mkdir(mode=0o700)
        run(["/usr/bin/ditto","-x","-k",str(archive),str(roundtrip)],"extract-delivery-archive")
        extracted=roundtrip/"METIS.app"
        members=("Contents/MacOS/metis-desktop", "Contents/Resources/bin/metis",
                 "Contents/Resources/bin/metis-runtime.json")
        for member in members:
            if not (extracted/member).is_file() or sha(extracted/member)!=sha(candidate/member):
                raise RuntimeError("archive roundtrip changed "+member)
        run(["/usr/bin/codesign","--verify","--deep","--strict",str(extracted)],"verify-extracted-signature")
        run([str(extracted/"Contents/MacOS/metis-desktop"),"--runtime-check"],"verify-extracted-runtime")
        diagnostic=json.loads((output/"verify-extracted-runtime.log").read_text())
        if diagnostic.get("sha256")!=runtime["sha256"] or diagnostic.get("version")!=runtime["version"]:
            raise RuntimeError("extracted runtime diagnostic does not match tested runtime")
        (output/(archive.name+".sha256")).write_text(sha(archive)+"  "+archive.name+"\n")
        report["artifact"]={"path":archive.name,"sha256":sha(archive),"bytes":archive.stat().st_size,
            "contains_standalone_cli_outside_app":False,"installed_on_host":False,"notarized":False}
        report["live_report"]="live-desktop/verification-report.json"
        report["passed"]=True
    except BaseException as error:
        report.update({"failure_type":type(error).__name__,"failure":str(error)})
    finally:
        # Failed candidates are never retained as usable binaries. Successful
        # delivery retains only the ZIP, not the tested application or CLI.
        if not report["passed"] and archive is not None:
            archive.unlink(missing_ok=True)
        remaining=stop_owned_processes(root)
        shutil.rmtree(root,ignore_errors=True)
        report["cleanup"]={"source_snapshot_destroyed":not source.exists(),"test_root_destroyed":not root.exists(),
            "all_unpacked_test_executables_destroyed":not root.exists(),"owned_process_pids_remaining":remaining}
        unchanged=all(Path(path).is_file() and sha(Path(path))==digest
            for path,digest in report["installed_binaries_before"].items())
        report["installed_binaries_hashes_unchanged"]=unchanged
        if not unchanged or root.exists() or remaining:report["passed"]=False
        if not report["passed"] and archive is not None:
            archive.unlink(missing_ok=True)
            archive.with_name(archive.name+".sha256").unlink(missing_ok=True)
        report["finished_utc"]=stamp()
        save(output/"delivery-verification.json",report)
        progress("complete" if report["passed"] else "failed")
    return 0 if report["passed"] else 1


if __name__=="__main__":
    raise SystemExit(main())
