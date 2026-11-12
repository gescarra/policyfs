# Use cases

This page collects practical configuration patterns. Each snippet is a complete `mounts:` block - paste it into `/etc/pfs/pfs.yaml` (replacing the example that ships with the package) and adjust the paths to match your disks.

```yaml
# /etc/pfs/pfs.yaml - paste one of the configs below here
fuse:
  allow_other: true # required if Plex/Jellyfin run as a different user

mounts:
  # ... paste a use-case config here
```

## 1) Media library (SSD for fast writes, HDD for capacity)

**Goal:** Put new writes on SSDs, keep reads/listings unified, then move older files to HDDs.

!!! note "About indexed storage"
`indexed: true` is optional and only affects metadata operations (like directory listing). File content reads still come from the owning disk, and maintenance jobs will spin disks by design.

**Recommended folder layout**

Keep the same relative layout on every disk:

```text
/mnt/ssd1/media/ingest/...
/mnt/ssd2/media/ingest/...
/mnt/ssd1/media/library/{movies,tv,music}/...
/mnt/ssd2/media/library/{movies,tv,music}/...
/mnt/hdd1/media/library/{movies,tv,music}/...
/mnt/hdd2/media/library/{movies,tv,music}/...
/mnt/hdd3/media/library/{movies,tv,music}/...
```

This pairs well with `path_preserving: true`.

**Suggested config:**

```yaml
mounts:
  media:
    storage_paths:
      - { id: ssd1, path: /mnt/ssd1/media, indexed: false }
      - { id: ssd2, path: /mnt/ssd2/media, indexed: false }
      - { id: hdd1, path: /mnt/hdd1/media, indexed: true }
      - { id: hdd2, path: /mnt/hdd2/media, indexed: true }
      - { id: hdd3, path: /mnt/hdd3/media, indexed: true }
    storage_groups:
      ssds: [ssd1, ssd2]
      hdds: [hdd1, hdd2, hdd3]
    routing_rules:
      - match: 'library/music/**'
        targets: [ssds]
        write_policy: most_free
        path_preserving: true
      - match: 'library/{movies,tv}/**'
        read_targets: [ssds, hdds]
        write_targets: [ssds]
        write_policy: most_free
        path_preserving: true
      - match: 'ingest/**'
        targets: [ssds]
        write_policy: most_free
        path_preserving: true
      - match: '**'
        read_targets: [ssds, hdds]
        write_targets: [ssds]
        write_policy: most_free
        path_preserving: true
    mover:
      enabled: true
      jobs:
        - name: ssd-to-hdd
          trigger:
            type: usage
            threshold_start: 80 # start moving when any source disk reaches 80% full
            threshold_stop: 70 # stop moving when all source disks are below 70% full
          source:
            groups: [ssds]
            patterns: ['library/{movies,tv}/**']
          destination:
            groups: [hdds]
            policy: most_free
            path_preserving: true
          conditions:
            min_age: 24h
```

**Tips:** Mark HDDs as `indexed: true` to reduce metadata disk touches. Use mover thresholds so the SSDs do not fill up.

## 2) NVR / CCTV storage (tiered retention)

**Goal:** Write new footage to SSD (IOPS), then archive to HDD after a minimum age.

**Suggested config:**

```yaml
mounts:
  nvr:
    storage_paths:
      - { id: ssd1, path: /mnt/ssd1/nvr, indexed: false }
      - { id: ssd2, path: /mnt/ssd2/nvr, indexed: false }
      - { id: hdd1, path: /mnt/hdd1/nvr, indexed: false }
      - { id: hdd2, path: /mnt/hdd2/nvr, indexed: false }
    storage_groups:
      ssds: [ssd1, ssd2]
      hdds: [hdd1, hdd2]
    routing_rules:
      - match: 'clips/**'
        targets: [ssds]
        write_policy: most_free
        path_preserving: true
      - match: 'exports/**'
        targets: [ssds]
        write_policy: most_free
        path_preserving: true
      - match: 'recordings/**'
        read_targets: [ssds, hdds]
        write_targets: [ssds]
        write_policy: most_free
        path_preserving: true
      - match: '**'
        read_targets: [ssds, hdds]
        write_targets: [ssds]
        write_policy: most_free
        path_preserving: true
    mover:
      enabled: true
      jobs:
        - name: archive-footage
          trigger:
            type: manual
          source:
            groups: [ssds]
            patterns: ['recordings/**']
          destination:
            groups: [hdds]
            policy: most_free
            path_preserving: true
          conditions:
            min_age: 24h
```

**Tips:** Start with `trigger.type: manual` and run `pfs move <mount> --job archive-footage` from a timer you control (e.g., systemd timer, cron, etc.).

## 3) Mergerfs alternative (simplest starting point)

**Goal:** Present multiple disks as a single mountpoint, while distributing writes. If you're new to PolicyFS, start here - no indexing, no tiering, just pooling.

**Suggested config:**

```yaml
mounts:
  media:
    storage_paths:
      - { id: d1, path: /mnt/disk1/media, indexed: false }
      - { id: d2, path: /mnt/disk2/media, indexed: false }
      - { id: d3, path: /mnt/disk3/media, indexed: false }
    storage_groups:
      disks: [d1, d2, d3]
    routing_rules:
      - match: '**'
        read_targets: [disks]
        write_targets: [disks]
        write_policy: most_free
        path_preserving: true
```

## 4) Redundant copies (mirroring)

**Goal:** Land new files on a fast disk, then keep a copy on each of two HDDs and an S3 mount, like RAID1 across independent disks.

**Suggested config:**

```yaml
mounts:
  media:
    storage_paths:
      - { id: ssd, path: /mnt/ssd/media, indexed: false }
      - { id: hdd1, path: /mnt/hdd1/media, indexed: true }
      - { id: hdd2, path: /mnt/hdd2/media, indexed: true }
      - { id: s3, path: /mnt/s3/media, indexed: true }
    storage_groups:
      hdds: [hdd1, hdd2]
      remote: [s3]
    routing_rules:
      - match: '**'
        read_targets: [ssd, hdds, remote]
        write_targets: [ssd]
    mover:
      jobs:
        - name: mirror
          trigger:
            type: manual
          source:
            paths: [ssd]
            patterns: ['**']
          destination:
            groups: [hdds, remote]
            mode: mirror
          conditions:
            min_age: 7d
          delete_source: true
```

**Tips:** Set `delete_source: false` to also keep the SSD copy. Run `pfs move media --job mirror` again at any time; it only copies what is missing or stale.
