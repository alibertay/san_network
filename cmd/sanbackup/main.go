// Command sanbackup backs up, restores and validates SAN node data
// directories. See docs/backup-restore.md.
//
//	sanbackup backup   --data DIR --out DIR [--backend lmdb] [--db FILE] [--key FILE]
//	sanbackup restore  --in DIR --data DIR
//	sanbackup validate --data DIR [--backend lmdb] [--db FILE]
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alibertay/san_network/internal/backup"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  sanbackup backup   --data DIR --out DIR [--backend lmdb|memory] [--db FILE] [--key FILE]
  sanbackup restore  --in DIR --data DIR
  sanbackup validate --data DIR [--backend lmdb|memory] [--db FILE]

Backs up the consensus database, identity key and peer cache with SHA-256
verification, restores them into a fresh data directory, and validates a
restored database (open, canonical chain, state root, finality checkpoint).
Stopping the node before backup is recommended; use SIGTERM and wait.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "backup":
		err = runBackup(os.Args[2:])
	case "restore":
		err = runRestore(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sanbackup:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string, names map[string]*string) error {
	for index := 0; index < len(args); index++ {
		name := args[index]
		target, ok := names[name]
		if !ok {
			return fmt.Errorf("unknown argument %q", name)
		}
		if index+1 >= len(args) {
			return fmt.Errorf("%s requires a value", name)
		}
		index++
		*target = args[index]
	}
	return nil
}

func runBackup(args []string) error {
	var dataDir, outDir, dbFile, keyFile, backend string
	if err := parseFlags(args, map[string]*string{
		"--data": &dataDir, "--out": &outDir, "--db": &dbFile, "--key": &keyFile, "--backend": &backend,
	}); err != nil {
		return err
	}
	if dataDir == "" || outDir == "" {
		return fmt.Errorf("backup requires --data and --out")
	}
	options := backup.DefaultOptions(dataDir)
	if dbFile != "" {
		options.DBFile = dbFile
	}
	if keyFile != "" {
		options.KeyFile = keyFile
	}
	if backend != "" {
		options.Backend = backend
	}
	if !filepath.IsAbs(outDir) {
		absolute, err := filepath.Abs(outDir)
		if err != nil {
			return err
		}
		outDir = absolute
	}
	manifest, err := backup.Backup(options, outDir)
	if err != nil {
		return err
	}
	return printJSON(manifest)
}

func runRestore(args []string) error {
	var inDir, dataDir string
	if err := parseFlags(args, map[string]*string{"--in": &inDir, "--data": &dataDir}); err != nil {
		return err
	}
	if inDir == "" || dataDir == "" {
		return fmt.Errorf("restore requires --in and --data")
	}
	manifest, err := backup.Restore(inDir, dataDir)
	if err != nil {
		return err
	}
	if _, err := backup.ValidateDataDir(dataDir, manifest.Backend, ""); err != nil {
		return fmt.Errorf("restored database failed validation: %w", err)
	}
	return printJSON(manifest)
}

func runValidate(args []string) error {
	var dataDir, dbFile, backend string
	if err := parseFlags(args, map[string]*string{"--data": &dataDir, "--db": &dbFile, "--backend": &backend}); err != nil {
		return err
	}
	if dataDir == "" {
		return fmt.Errorf("validate requires --data")
	}
	report, err := backup.ValidateDataDir(dataDir, backend, dbFile)
	if err != nil {
		return err
	}
	return printJSON(report)
}

func printJSON(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}
