package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parsed is an rpm file read back by code that shares nothing with the writer
// but the file format.
type parsed struct {
	leadName   string
	leadArch   uint16
	signature  map[uint32]headerValue
	main       map[uint32]headerValue
	mainBytes  []byte
	payloadRaw []byte
}

type headerValue struct {
	kind  uint32
	count uint32
	data  []byte
}

func readHeader(t *testing.T, file []byte, offset int) (map[uint32]headerValue, int) {
	t.Helper()

	if !bytes.Equal(file[offset:offset+8], []byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0}) {
		t.Fatalf("no header magic at %d", offset)
	}

	entries := int(binary.BigEndian.Uint32(file[offset+8:]))
	size := int(binary.BigEndian.Uint32(file[offset+12:]))
	index := file[offset+16 : offset+16+16*entries]
	store := file[offset+16+16*entries : offset+16+16*entries+size]

	values := map[uint32]headerValue{}

	for i := 0; i < entries; i++ {
		one := index[16*i : 16*i+16]
		tag, kind, start, count := binary.BigEndian.Uint32(one), binary.BigEndian.Uint32(one[4:]), int(binary.BigEndian.Uint32(one[8:])), binary.BigEndian.Uint32(one[12:])

		if start > len(store) {
			t.Fatalf("tag %d points outside the store", tag)
		}

		switch kind {
		case typeInt32:
			if start%4 != 0 {
				t.Errorf("tag %d: an int32 at an unaligned offset %d", tag, start)
			}
		case typeInt16:
			if start%2 != 0 {
				t.Errorf("tag %d: an int16 at an unaligned offset %d", tag, start)
			}
		}

		values[tag] = headerValue{kind: kind, count: count, data: store[start:]}
	}

	return values, 16 + 16*entries + size
}

func (v headerValue) str() string {
	return string(v.data[:bytes.IndexByte(v.data, 0)])
}

func (v headerValue) strs() []string {
	var out []string

	rest := v.data

	for i := uint32(0); i < v.count; i++ {
		end := bytes.IndexByte(rest, 0)
		out = append(out, string(rest[:end]))
		rest = rest[end+1:]
	}

	return out
}

func (v headerValue) ints() []uint32 {
	out := make([]uint32, v.count)
	for i := range out {
		if v.kind == typeInt16 {
			out[i] = uint32(binary.BigEndian.Uint16(v.data[2*i:]))
		} else {
			out[i] = binary.BigEndian.Uint32(v.data[4*i:])
		}
	}

	return out
}

func parse(t *testing.T, file []byte) parsed {
	t.Helper()

	if !bytes.Equal(file[:4], []byte{0xed, 0xab, 0xee, 0xdb}) || len(file) < 96 {
		t.Fatal("no rpm lead")
	}

	result := parsed{leadName: strings.TrimRight(string(file[10:76]), "\x00"), leadArch: binary.BigEndian.Uint16(file[8:])}

	signature, used := readHeader(t, file, 96)
	result.signature = signature

	offset := 96 + used
	if offset%8 != 0 {
		offset += 8 - offset%8
	}

	main, mainSize := readHeader(t, file, offset)
	result.main = main
	result.mainBytes = file[offset : offset+mainSize]
	result.payloadRaw = file[offset+mainSize:]

	return result
}

func samplePackage() Package {
	return Package{
		Name: "tailcove-agent", Version: "0.2.0", Release: "1", Arch: "x86_64",
		Summary: "Tailcove agent", Description: "Sends logs.", License: "MIT", URL: "https://tailcove.dev", Packager: "Rana Tayyab <developers@akcybex.com>",
		BuildTime: time.Unix(1761561600, 0),
		PostIn:    "#!/bin/sh\necho installed\n", PreUn: "#!/bin/sh\necho removing\n",
		Entries: []Entry{
			{Path: "/usr/bin/tailcove-agent", Mode: 0o755, Data: []byte("binary bytes")},
			{Path: "/usr/lib/systemd/system/tailcove-agent.service", Mode: 0o644, Data: []byte("[Unit]\n")},
			{Path: "/usr/share/doc/tailcove-agent", Mode: 0o755, Dir: true},
			{Path: "/usr/share/doc/tailcove-agent/README.md", Mode: 0o644, Data: []byte("# readme\n"), Doc: true},
			{Path: "/etc/tailcove-agent.example", Mode: 0o640, Data: []byte("{}"), Config: true},
		},
	}
}

func TestTheFileIsInternallyConsistent(t *testing.T) {
	file, err := Build(samplePackage())
	if err != nil {
		t.Fatal(err)
	}

	p := parse(t, file)

	if p.leadName != "tailcove-agent-0.2.0-1" || p.leadArch != 1 {
		t.Errorf("lead %q %d", p.leadName, p.leadArch)
	}

	// The signature header describes the main header and the payload.
	whole := md5.New()
	whole.Write(p.mainBytes)
	whole.Write(p.payloadRaw)

	if got := p.signature[tagSigMD5].data[:16]; !bytes.Equal(got, whole.Sum(nil)) {
		t.Error("the MD5 does not match the header and payload")
	}

	if got := p.signature[tagSigSize].ints()[0]; int(got) != len(p.mainBytes)+len(p.payloadRaw) {
		t.Errorf("size %d, want %d", got, len(p.mainBytes)+len(p.payloadRaw))
	}

	sum1, sum256 := sha1.Sum(p.mainBytes), sha256.Sum256(p.mainBytes)

	if p.signature[tagSigSHA1].str() != hex.EncodeToString(sum1[:]) || p.signature[tagSigSHA256].str() != hex.EncodeToString(sum256[:]) {
		t.Error("the header digests do not match")
	}

	// The payload decompresses to a cpio archive of the declared size.
	zipped, err := gzip.NewReader(bytes.NewReader(p.payloadRaw))
	if err != nil {
		t.Fatal(err)
	}

	cpio, err := io.ReadAll(zipped)
	if err != nil || int(p.signature[tagSigPayloadSize].ints()[0]) != len(cpio) {
		t.Fatalf("payload %d bytes, declared %d (%v)", len(cpio), p.signature[tagSigPayloadSize].ints()[0], err)
	}

	files := readCpio(t, cpio)

	// Every file of the header is in the archive with the right bytes and digest.
	dirs, bases, indexes := p.main[tagDirNames].strs(), p.main[tagBaseNames].strs(), p.main[tagDirIndexes].ints()
	digests, modes, sizes := p.main[tagFileDigests].strs(), p.main[tagFileModes].ints(), p.main[tagFileSizes].ints()

	if len(bases) != 5 {
		t.Fatalf("%d files listed", len(bases))
	}

	for i, base := range bases {
		full := dirs[indexes[i]] + base
		content, inArchive := files["."+full]

		if !inArchive {
			t.Errorf("%s is listed but not in the archive", full)

			continue
		}

		if modes[i]&0o040000 != 0 {
			if digests[i] != "" {
				t.Errorf("%s: a directory has a digest", full)
			}

			continue
		}

		sum := sha256.Sum256(content.data)
		if digests[i] != hex.EncodeToString(sum[:]) || int(sizes[i]) != len(content.data) {
			t.Errorf("%s: digest or size does not match", full)
		}

		if content.mode&0o777 != modes[i]&0o777 {
			t.Errorf("%s: archive mode %o, header mode %o", full, content.mode, modes[i])
		}
	}

	// The metadata a package manager shows.
	if p.main[tagName].str() != "tailcove-agent" || p.main[tagVersion].str() != "0.2.0" || p.main[tagRelease].str() != "1" || p.main[tagArch].str() != "x86_64" {
		t.Error("name, version, release or architecture is wrong")
	}

	if p.main[tagPostIn].str() != "#!/bin/sh\necho installed\n" || p.main[tagPostInProg].str() != "/bin/sh" || p.main[tagPreUn].str() == "" {
		t.Error("scriptlets are missing")
	}

	if _, has := p.main[tagPreIn]; has {
		t.Error("an empty scriptlet was written")
	}

	if flags := p.main[tagFileFlags].ints(); flags[0]&flagConfig == 0 || flags[0]&flagNoReplace == 0 {
		t.Errorf("the config file is not marked: %v", flags)
	}

	if names := p.main[tagRequireName].strs(); names[1] != "rpmlib(CompressedFileNames)" {
		t.Errorf("requires %v", names)
	}
}

type cpioFile struct {
	mode uint32
	data []byte
}

func readCpio(t *testing.T, archive []byte) map[string]cpioFile {
	t.Helper()

	files := map[string]cpioFile{}
	offset := 0

	for {
		if string(archive[offset:offset+6]) != "070701" {
			t.Fatalf("bad cpio magic at %d", offset)
		}

		field := func(n int) uint32 {
			value, err := strconv.ParseUint(string(archive[offset+6+8*n:offset+14+8*n]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}

			return uint32(value)
		}

		mode, size, nameSize := field(1), int(field(6)), int(field(11))
		nameStart := offset + 110
		name := string(archive[nameStart : nameStart+nameSize-1])

		next := nameStart + nameSize
		for next%4 != 0 {
			next++
		}

		if name == "TRAILER!!!" {
			return files
		}

		files[name] = cpioFile{mode: mode, data: archive[next : next+size]}

		next += size
		for next%4 != 0 {
			next++
		}

		offset = next
	}
}

func TestBuildsAreReproducibleForTheSameTime(t *testing.T) {
	first, _ := Build(samplePackage())
	second, _ := Build(samplePackage())

	if !bytes.Equal(first, second) {
		t.Error("two builds of the same package differ")
	}
}

func TestInvalidPackagesAreRefused(t *testing.T) {
	for name, change := range map[string]func(*Package){
		"no name":          func(p *Package) { p.Name = "" },
		"dash in version":  func(p *Package) { p.Version = "0.2.0-rc.0" },
		"unknown arch":     func(p *Package) { p.Arch = "sparc" },
		"slash in release": func(p *Package) { p.Release = "1/2" },
	} {
		pkg := samplePackage()
		change(&pkg)

		if _, err := Build(pkg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestIndexIsSortedAndTheStoreKeepsItsAlignment(t *testing.T) {
	file, _ := Build(samplePackage())
	p := parse(t, file)

	previous := uint32(0)

	if len(p.main) < 30 {
		t.Errorf("only %d tags", len(p.main))
	}

	// readHeader already failed the test on a misaligned int; check the order in the raw index.
	entries := int(binary.BigEndian.Uint32(p.mainBytes[8:]))

	for i := 0; i < entries; i++ {
		tag := binary.BigEndian.Uint32(p.mainBytes[16+16*i:])
		if tag < previous {
			t.Fatalf("tag %d after %d: the index is not sorted", tag, previous)
		}

		previous = tag
	}
}
