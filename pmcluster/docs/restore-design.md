# Restore design (Phase 5 — design only, not implemented)

`pmcluster backup create` and the `backup_before_deploy: true` DSL hook
let operators take on-demand snapshots of every Docker volume on the
local node (via the offen `docker exec backup` path). The complementary
restore path is intentionally **not implemented in Phase 5** — restoring
volumes safely is a different problem from taking a copy of them, and
it deserves its own deliberate design.

This file sketches the approach we'll take when we get to it.

---

## Constraints

- **A volume cannot be restored while it's mounted.** Containers using the
  volume must stop first. A naive `tar -xzf` on top of a live volume
  produces split-brain corruption (writes from two writers to the same
  path).
- **offen archives are tarballs of `/var/lib/docker/volumes/<name>/_data`.**
  Each archive holds the volumes from one node, all at once. We don't
  index per-volume offsets; restore granularity is per-archive.
- **Multi-node restore needs to happen on the right node.** Volumes are
  per-host; the archive on node A holds node A's volumes only. pmcluster
  knows which manager it runs on but doesn't currently SSH to workers.
  Phase-1 restore is **manager-only**.

## Proposed flow (single volume)

```
pmcluster restore <stack> <volume> --from-backup-id=<id>
```

1. Resolve the backup id → archive path (`/var/backups/docker-volumes/<file>`).
   Refuse if the file doesn't exist on the local host.
2. Find the consuming services for `<stack>_*` that mount `<volume>`. Use
   the docker SDK to list services + mounts; this is read-only.
3. Scale those services to 0 replicas. Wait until the tasks actually
   exit (poll `docker service ps`).
4. Untar the archive into `/var/lib/docker/volumes/<stack>_<volume>/_data`,
   replacing existing contents. Use a temp directory + atomic rename to
   minimise the window where the volume is in an inconsistent state.
5. Scale services back to their previous replica counts (recorded in
   step 3).
6. Record the restore in a new `restores` audit table.

## Proposed flow (whole stack)

```
pmcluster restore <stack> --from-backup-id=<id>
```

1. `docker stack rm <stack>`, wait for full teardown.
2. For each volume in the stack: untar from the archive (same atomic
   rename pattern).
3. Re-deploy the stack using its current revision's rendered YAML.

## Open questions

- **Version-skew safety.** If the backup was taken under image `v1` and
  the current stack rendered YAML targets `v2`, do we re-deploy at the
  current revision or at the closest revision before the backup
  timestamp? Recommendation: roll the stack back to the rendered YAML
  closest in time to (and not after) the backup, then run the deploy.
- **Worker volumes.** Punted: workers run their own offen tasks and
  archive locally; pmcluster only restores manager-local volumes in
  Phase 1. A v2 design adds an SSH-out path with the manager's join-token
  helper.
- **Cross-host archive transport.** If an operator wants to restore from
  an archive on a different node, they must `scp` it first. Documented,
  not automated.
- **Backups in S3.** The bundled offen stack has commented-out S3 envs;
  if those are enabled, archives may not exist on the local filesystem
  at all. Restore would need to pull from S3 first. Add an `--from-s3`
  flag in v2.

## What pmcluster gives us today

- `pmcluster backup list` → audit log of every triggered backup with
  archive paths.
- `pmcluster backup create` → on-demand snapshot.
- `backup_before_deploy: true` in the DSL → automatic snapshot before a
  deploy.

A working restore is a `tar` invocation away on the host today; we just
haven't wrapped it in a verified, audit-logged command yet.
