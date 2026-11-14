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

## GitHub Releases

GitHub Actions runs tests and `go vet` on pushes and pull requests. To publish
release binaries for Linux, macOS, and Windows, create and push a version tag:

```sh
git tag v1.0.0
git push origin v1.0.0
```

The workflow builds amd64 and arm64 binaries and creates a GitHub Release with
`.tar.gz` archives for Linux and macOS and `.zip` archives for Windows.

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

To merge the current full and differential archives into a new full backup and
start a new backup chain, run:

```sh
filedelta compact -backup-dir /srv/backups/site
```

The new full archive is added without deleting the previous archives.
To delete the full and differential archives in the previous chain after the
new full archive has been created and registered, add `-delete-old`:

```sh
filedelta compact -backup-dir /srv/backups/site -delete-old
```

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
