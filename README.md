# FileDelta

FileDelta is a dependency-free Go command-line tool for full and differential
file backups. It creates `.tar.gz` archives and stores backup history in
`.filedelta/state.json` under the backup directory.

## Build

Build for the operating system and CPU architecture of the backup server:

```sh
go build -o filedelta ./cmd/filedelta
```

For example, build a Linux x86-64 binary from another platform with:

```sh
GOOS=linux GOARCH=amd64 go build -o filedelta ./cmd/filedelta
```

## Project layout

```text
cmd/filedelta/       CLI entry point
internal/filedelta/  CLI and backup implementation
```

## VS Code

Open `FileDelta.code-workspace` in VS Code. Press `Ctrl+Shift+B` to run the
native build task, or select `Go: Test` from the Tasks menu. The Go extension
(`golang.go`) is recommended.

## Commands

Create the initial full backup:

```sh
filedelta full -source /srv/www/site -backup-dir /srv/backups/site
```

Create a differential archive after the initial full backup:

```sh
filedelta diff -source /srv/www/site -backup-dir /srv/backups/site
```

Rebuild the current state from the full archive and all recorded differentials:

```sh
filedelta latest -backup-dir /srv/backups/site
```

This writes or replaces `latest.tar.gz`. The archive contains the contents of
the source folder at the time the differential chain was last updated. If no
files have changed, `diff` reports that and does not create an archive.

Run `filedelta --help` for command syntax. The backup directory must be outside
the source folder. A new `full` starts a new chain; existing archives are kept.
Differential deletions are recorded in `.filedelta/state.json` and applied when
building the latest-state archive. Keep this metadata directory with the
archives.

FileDelta takes an exclusive lock during each command. If a process is
terminated unexpectedly, it may leave `.filedelta/lock`; after confirming no
FileDelta process is running, remove that lock file before retrying.

## File metadata

Archive entries preserve file contents, relative paths, basic permission bits,
and modification times. ACLs, extended attributes, ownership, and special files
such as sockets and device nodes are not supported. Symlinks are stored as links
and are not followed while scanning.
