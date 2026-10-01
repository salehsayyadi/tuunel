# Docker configuration directories

Put one directory per role here (mounted read-only at `/etc/tuunel`):

```
deploy/docker/edge/config.yaml     # e.g. configs/reverse-edge.yaml, api.socket: /run/tuunel/tuunel.sock
deploy/docker/edge/node.key        # tuunel genkey, owner 10001 (container user), mode 0600
deploy/docker/remote/config.yaml
deploy/docker/remote/node.key
```

Generate a key without installing anything:

```
docker run --rm tuunel genkey > deploy/docker/edge/node.key
docker run --rm -v "$PWD/deploy/docker/edge:/etc/tuunel:ro" tuunel pubkey -key /etc/tuunel/node.key
sudo chown 10001:10001 deploy/docker/edge/node.key && sudo chmod 0600 deploy/docker/edge/node.key
```

Key files are ignored by git (`deploy/docker/*/node.key`).
