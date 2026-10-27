// Command mkrpm builds an rpm package without rpmbuild: the agent is released from
// a runner that has no rpm tooling, and the format is small enough to write.
//
//	mkrpm -out dist/x.rpm -name tailcove-agent -version 0.2.0 -release 1 -arch x86_64 \
//	      -manifest files.json -postin post.sh -preun preun.sh ...
//
// The manifest is a JSON list of {"src": path, "dest": install path, "mode": "0755",
// "config": false, "doc": false, "dir": false} entries; "src" is empty for a directory.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

type manifestEntry struct {
	Src    string `json:"src"`
	Dest   string `json:"dest"`
	Mode   string `json:"mode"`
	Config bool   `json:"config"`
	Doc    bool   `json:"doc"`
	Dir    bool   `json:"dir"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mkrpm:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("mkrpm", flag.ContinueOnError)

	var (
		out, name, version, release, arch, summary, description, license, url, vendor, packager string
		manifest, preIn, postIn, preUn, postUn                                                  string
		epoch                                                                                   int64
	)

	flags.StringVar(&out, "out", "", "the rpm file to write")
	flags.StringVar(&name, "name", "", "package name")
	flags.StringVar(&version, "version", "", "version (no dashes)")
	flags.StringVar(&release, "release", "1", "release (no dashes)")
	flags.StringVar(&arch, "arch", "x86_64", "x86_64 or aarch64")
	flags.StringVar(&summary, "summary", "", "one line summary")
	flags.StringVar(&description, "description", "", "description")
	flags.StringVar(&license, "license", "MIT", "license")
	flags.StringVar(&url, "url", "", "home page")
	flags.StringVar(&vendor, "vendor", "", "vendor")
	flags.StringVar(&packager, "packager", "", "packager")
	flags.StringVar(&manifest, "manifest", "", "JSON list of the files")
	flags.StringVar(&preIn, "prein", "", "script run before install")
	flags.StringVar(&postIn, "postin", "", "script run after install")
	flags.StringVar(&preUn, "preun", "", "script run before removal")
	flags.StringVar(&postUn, "postun", "", "script run after removal")
	flags.Int64Var(&epoch, "time", 0, "build time as unix seconds (default now; set it for a reproducible file)")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if out == "" || manifest == "" {
		return fmt.Errorf("-out and -manifest are required")
	}

	raw, err := readFile(manifest)
	if err != nil {
		return err
	}

	var listed []manifestEntry
	if err := json.Unmarshal(raw, &listed); err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}

	pkg := Package{
		Name: name, Version: version, Release: release, Arch: arch,
		Summary: summary, Description: description, License: license, URL: url, Vendor: vendor, Packager: packager,
	}

	if epoch > 0 {
		pkg.BuildTime = time.Unix(epoch, 0)
	}

	for _, one := range listed {
		mode, err := strconv.ParseUint(one.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("%s: mode %q is not octal", one.Dest, one.Mode)
		}

		entry := Entry{Path: one.Dest, Mode: uint32(mode), Dir: one.Dir, Config: one.Config, Doc: one.Doc}

		if !one.Dir {
			if entry.Data, err = readFile(one.Src); err != nil {
				return err
			}
		}

		pkg.Entries = append(pkg.Entries, entry)
	}

	for target, file := range map[*string]string{&pkg.PreIn: preIn, &pkg.PostIn: postIn, &pkg.PreUn: preUn, &pkg.PostUn: postUn} {
		if file == "" {
			continue
		}

		body, err := readFile(file)
		if err != nil {
			return err
		}

		*target = string(body)
	}

	built, err := Build(pkg)
	if err != nil {
		return err
	}

	return os.WriteFile(out, built, 0o644)
}
