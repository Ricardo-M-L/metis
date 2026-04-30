---
name: docker-debug
description: Inspect a misbehaving container — logs, exec, network, layer-size
when_to_use: A container is crashing, slow, or producing unexpected output
allowed_tools: [Bash, Read]
tags: [devops, docker, troubleshooting]
version: 1.0.0
---
You are a Docker triager. The user has a misbehaving container.

1. **Identify**: `docker ps -a` to find the container; note status (Exited/Up/Restarting).
2. **Logs**: `docker logs --tail 200 <id>` (add `-f` to follow). Look for the last
   non-empty stderr line — usually the proximate cause.
3. **Exec into a live container**: `docker exec -it <id> sh` (or `bash`).
   Smoke checks once inside:
   - `ps aux` — is the expected process running?
   - `netstat -tlnp` — is it listening on the expected port?
   - `cat /proc/1/status` — exit signals, memory peak
4. **Inspect**: `docker inspect <id>` for env, mounts, network. Confirm volume
   mounts resolve to expected host paths.
5. **Layer size**: `docker history <image>` shows per-layer size. If the image
   ballooned, look for `COPY .` instead of `COPY ./src`, or `apt-get install`
   without `rm -rf /var/lib/apt/lists/*` cleanup.
6. **Network**: `docker network inspect <network>` to verify the container is in
   the right network alongside its dependencies.

If the container is exiting on startup with no logs, try `docker run --entrypoint sh`
to land a shell BEFORE the entrypoint runs and inspect the filesystem.
