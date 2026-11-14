package filedelta

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
)

// Run executes the FileDelta command-line application.
func Run(args []string, stdout, stderr io.Writer) error {
	return run(args, stdout, stderr)
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		return nil
	}

	command := args[0]
	if command != "full" && command != "diff" && command != "latest" && command != "compact" {
		return fmt.Errorf("unknown command %q (use --help)", command)
	}

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "source folder to back up (required for full and diff)")
	backupDir := flags.String("backup-dir", "", "directory in which to store backups (required)")
	deleteOld := flags.Bool("delete-old", false, "delete previous full and differential archives after compact")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *deleteOld && command != "compact" {
		return errors.New("-delete-old is only valid for compact")
	}
	if *backupDir == "" {
		return errors.New("-backup-dir is required")
	}

	backupPath, err := filepath.Abs(*backupDir)
	if err != nil {
		return fmt.Errorf("resolve backup directory: %w", err)
	}

	switch command {
	case "full":
		if *source == "" {
			return errors.New("-source is required for full")
		}
		sourcePath, err := prepareSource(*source, backupPath)
		if err != nil {
			return err
		}
		name, err := createFull(sourcePath, backupPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Full backup created: %s\n", name)
	case "diff":
		if *source == "" {
			return errors.New("-source is required for diff")
		}
		sourcePath, err := prepareSource(*source, backupPath)
		if err != nil {
			return err
		}
		name, changed, err := createDiff(sourcePath, backupPath)
		if err != nil {
			return err
		}
		if !changed {
			fmt.Fprintln(stdout, "No changes detected.")
			return nil
		}
		fmt.Fprintf(stdout, "Differential backup created: %s\n", name)
	case "latest":
		name, err := createLatest(backupPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Latest-state archive created: %s\n", name)
	case "compact":
		name, err := compactBackup(backupPath, *deleteOld)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Backup chain compacted into full archive: %s\n", name)
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `FileDelta - incremental file backup tool

Usage:
  filedelta full   -source <folder> -backup-dir <folder>
  filedelta diff   -source <folder> -backup-dir <folder>
  filedelta latest -backup-dir <folder>
  filedelta compact -backup-dir <folder>
      [-delete-old]

Commands:
  full    Create a full archive and start a new backup chain.
  diff    Archive additions and changes since the last full/diff; record deletions.
  latest  Rebuild the current state from the chain and write latest.tar.gz.
  compact Rebuild the current state as a new full archive and start a new chain.
          -delete-old Remove previous full and differential archives after success.

The backup directory must not be inside the source folder.
Run a full backup before creating differential backups.`)
}
