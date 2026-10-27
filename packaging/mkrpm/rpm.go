package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Header tag types.
const (
	typeInt16       = 3
	typeInt32       = 4
	typeString      = 6
	typeBin         = 7
	typeStringArray = 8
	typeI18N        = 9
)

// Tags of the main header, and of the signature header.
const (
	tagI18NTable       = 100
	tagSigSHA1         = 269
	tagSigSHA256       = 273
	tagSigSize         = 1000
	tagSigMD5          = 1004
	tagSigPayloadSize  = 1007
	tagName            = 1000
	tagVersion         = 1001
	tagRelease         = 1002
	tagSummary         = 1004
	tagDescription     = 1005
	tagBuildTime       = 1006
	tagBuildHost       = 1007
	tagSize            = 1009
	tagVendor          = 1011
	tagLicense         = 1014
	tagPackager        = 1015
	tagURL             = 1020
	tagOS              = 1021
	tagArch            = 1022
	tagPreIn           = 1023
	tagPostIn          = 1024
	tagPreUn           = 1025
	tagPostUn          = 1026
	tagFileSizes       = 1028
	tagFileModes       = 1030
	tagFileRdevs       = 1033
	tagFileMtimes      = 1034
	tagFileDigests     = 1035
	tagFileLinkTos     = 1036
	tagFileFlags       = 1037
	tagFileUserName    = 1039
	tagFileGroupName   = 1040
	tagRPMVersion      = 1064
	tagFileDevices     = 1095
	tagFileInodes      = 1096
	tagFileLangs       = 1097
	tagPreInProg       = 1085
	tagPostInProg      = 1086
	tagPreUnProg       = 1087
	tagPostUnProg      = 1088
	tagRequireFlags    = 1048
	tagRequireName     = 1049
	tagRequireVersion  = 1050
	tagProvideName     = 1047
	tagProvideFlags    = 1112
	tagProvideVersion  = 1113
	tagDirIndexes      = 1116
	tagBaseNames       = 1117
	tagDirNames        = 1118
	tagPayloadFormat   = 1124
	tagPayloadCompress = 1125
	tagPayloadFlags    = 1126
	tagFileDigestAlgo  = 5011
	tagPayloadDigest   = 5092
	tagPayloadDigestAl = 5093

	// Regions: the tags a header was written with, which rpm keeps byte for byte
	// when it stores the header, so that its digests still match afterwards.
	regionSignature = 62
	regionImmutable = 63
)

// File flags.
const (
	flagConfig    = 1 << 0
	flagDoc       = 1 << 1
	flagNoReplace = 1 << 4
)

// Entry is one file or directory of the package.
type Entry struct {
	// Path is the absolute install path.
	Path string
	// Mode is the permission bits, e.g. 0o755.
	Mode uint32
	// Dir marks a directory owned by the package.
	Dir bool
	// Data is the content of a file.
	Data []byte
	// Config marks a configuration file; it is not replaced on upgrade.
	Config bool
	// Doc marks documentation.
	Doc bool
}

// Package is everything an rpm holds.
type Package struct {
	Name, Version, Release, Arch string
	Summary, Description         string
	License, URL, Vendor         string
	Packager                     string
	BuildTime                    time.Time
	Entries                      []Entry
	// Scriptlets run with /bin/sh.
	PreIn, PostIn, PreUn, PostUn string
}

var archNumbers = map[string]uint16{"x86_64": 1, "aarch64": 12, "noarch": 255}

// Build returns the bytes of the rpm file.
func Build(pkg Package) ([]byte, error) {
	if pkg.Name == "" || pkg.Version == "" || pkg.Release == "" {
		return nil, fmt.Errorf("a package needs a name, a version and a release")
	}

	if strings.ContainsAny(pkg.Name, " \t/") {
		return nil, fmt.Errorf("%q cannot be a package name", pkg.Name)
	}

	for _, field := range []string{pkg.Version, pkg.Release} {
		if strings.ContainsAny(field, "- \t/") {
			return nil, fmt.Errorf("%q cannot be part of a package version or release", field)
		}
	}

	archNumber, known := archNumbers[pkg.Arch]
	if !known {
		return nil, fmt.Errorf("unsupported architecture %q", pkg.Arch)
	}

	entries := append([]Entry(nil), pkg.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	built := pkg.BuildTime
	if built.IsZero() {
		built = time.Now()
	}

	payload, payloadSize, err := buildPayload(entries, built)
	if err != nil {
		return nil, err
	}

	header, err := mainHeader(pkg, entries, built, payload)
	if err != nil {
		return nil, err
	}

	signature := signatureHeader(header, payload, payloadSize)

	var file bytes.Buffer

	file.Write(lead(pkg, archNumber))
	file.Write(signature)
	file.Write(header)
	file.Write(payload)

	return file.Bytes(), nil
}

// lead is the 96 byte start of every rpm file.
func lead(pkg Package, archNumber uint16) []byte {
	out := make([]byte, 96)
	copy(out, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	binary.BigEndian.PutUint16(out[6:], 0) // binary package
	binary.BigEndian.PutUint16(out[8:], archNumber)
	copy(out[10:76], pkg.Name+"-"+pkg.Version+"-"+pkg.Release)
	binary.BigEndian.PutUint16(out[76:], 1) // Linux
	binary.BigEndian.PutUint16(out[78:], 5) // the signature is a header

	return out
}

// buildPayload is the gzip-compressed cpio archive (newc format) of the files.
func buildPayload(entries []Entry, built time.Time) (compressed []byte, rawSize int, err error) {
	var raw bytes.Buffer

	for index, entry := range entries {
		name := "." + entry.Path
		mode := uint32(0o100000) | entry.Mode
		size := len(entry.Data)
		links := 1

		if entry.Dir {
			mode, size, links = uint32(0o040000)|entry.Mode, 0, 2
		}

		writeCpio(&raw, uint32(index+1), mode, links, uint32(built.Unix()), entry.Data[:size], name)
	}

	writeCpio(&raw, 0, 0, 1, 0, nil, "TRAILER!!!")

	var out bytes.Buffer

	zipper, err := gzip.NewWriterLevel(&out, 9)
	if err != nil {
		return nil, 0, err
	}

	if _, err := zipper.Write(raw.Bytes()); err != nil {
		return nil, 0, err
	}

	if err := zipper.Close(); err != nil {
		return nil, 0, err
	}

	return out.Bytes(), raw.Len(), nil
}

func writeCpio(out *bytes.Buffer, inode, mode uint32, links int, mtime uint32, data []byte, name string) {
	fmt.Fprintf(out, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		inode, mode, 0, 0, links, mtime, len(data), 0, 0, 0, 0, len(name)+1, 0)

	out.WriteString(name)
	out.WriteByte(0)
	pad(out, 110+len(name)+1)

	out.Write(data)
	pad(out, len(data))
}

func pad(out *bytes.Buffer, length int) {
	for ; length%4 != 0; length++ {
		out.WriteByte(0)
	}
}

// header builds the binary header structure: magic, index and data store.
type header struct {
	tags []headerEntry
	// region is the tag of the immutable region that covers every entry (62 for
	// the signature header, 63 for the main one).
	region uint32
}

type headerEntry struct {
	tag, kind, count uint32
	data             []byte
}

func (h *header) add(tag, kind, count uint32, data []byte) {
	h.tags = append(h.tags, headerEntry{tag, kind, count, data})
}

func (h *header) addString(tag uint32, value string) {
	h.add(tag, typeString, 1, append([]byte(value), 0))
}

func (h *header) addI18N(tag uint32, value string) {
	h.add(tag, typeI18N, 1, append([]byte(value), 0))
}

func (h *header) addStrings(tag uint32, values []string) {
	var data []byte

	for _, value := range values {
		data = append(data, value...)
		data = append(data, 0)
	}

	h.add(tag, typeStringArray, uint32(len(values)), data)
}

func (h *header) addInt32(tag uint32, values ...uint32) {
	data := make([]byte, 4*len(values))
	for i, value := range values {
		binary.BigEndian.PutUint32(data[4*i:], value)
	}

	h.add(tag, typeInt32, uint32(len(values)), data)
}

func (h *header) addInt16(tag uint32, values ...uint16) {
	data := make([]byte, 2*len(values))
	for i, value := range values {
		binary.BigEndian.PutUint16(data[2*i:], value)
	}

	h.add(tag, typeInt16, uint32(len(values)), data)
}

func (h *header) addBin(tag uint32, data []byte) {
	h.add(tag, typeBin, uint32(len(data)), data)
}

// bytes returns the encoded header. Tags are sorted, and each value starts at
// the alignment its type needs. With a region, the first index entry is the
// region tag and a 16 byte trailer closes the data store: its offset is minus the
// size of the index that the region covers, which is how rpm tells that the
// header it reads is whole.
func (h *header) bytes() []byte {
	sort.Slice(h.tags, func(i, j int) bool { return h.tags[i].tag < h.tags[j].tag })

	var store bytes.Buffer

	entries := len(h.tags)
	if h.region != 0 {
		entries++
	}

	index := make([]byte, 0, 16*entries)

	appendEntry := func(tag, kind, offset, count uint32) {
		var one [16]byte
		binary.BigEndian.PutUint32(one[0:], tag)
		binary.BigEndian.PutUint32(one[4:], kind)
		binary.BigEndian.PutUint32(one[8:], offset)
		binary.BigEndian.PutUint32(one[12:], count)
		index = append(index, one[:]...)
	}

	for _, entry := range h.tags {
		switch entry.kind {
		case typeInt16:
			for store.Len()%2 != 0 {
				store.WriteByte(0)
			}
		case typeInt32:
			for store.Len()%4 != 0 {
				store.WriteByte(0)
			}
		}

		appendEntry(entry.tag, entry.kind, uint32(store.Len()), entry.count)
		store.Write(entry.data)
	}

	if h.region != 0 {
		trailerOffset := store.Len()

		var trailer [16]byte
		binary.BigEndian.PutUint32(trailer[0:], h.region)
		binary.BigEndian.PutUint32(trailer[4:], typeBin)
		binary.BigEndian.PutUint32(trailer[8:], uint32(-int32(16*entries)))
		binary.BigEndian.PutUint32(trailer[12:], 16)
		store.Write(trailer[:])

		// The region entry sorts before every other tag (62 and 63 are the smallest).
		region := make([]byte, 0, 16)
		saved := index
		index = region
		appendEntry(h.region, typeBin, uint32(trailerOffset), 16)
		index = append(index, saved...)
	}

	var out bytes.Buffer

	out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})

	var counts [8]byte
	binary.BigEndian.PutUint32(counts[0:], uint32(entries))
	binary.BigEndian.PutUint32(counts[4:], uint32(store.Len()))
	out.Write(counts[:])
	out.Write(index)
	out.Write(store.Bytes())

	return out.Bytes()
}

// signatureHeader holds the sizes and digests that let rpm check the file. It is
// padded to a multiple of eight bytes, as rpm expects.
func signatureHeader(main, payload []byte, payloadSize int) []byte {
	whole := md5.New()
	whole.Write(main)
	whole.Write(payload)

	sha1Sum := sha1.Sum(main)
	sha256Sum := sha256.Sum256(main)

	h := header{region: regionSignature}

	h.addInt32(tagSigSize, uint32(len(main)+len(payload)))
	h.addBin(tagSigMD5, whole.Sum(nil))
	h.addString(tagSigSHA1, hex.EncodeToString(sha1Sum[:]))
	h.addString(tagSigSHA256, hex.EncodeToString(sha256Sum[:]))
	h.addInt32(tagSigPayloadSize, uint32(payloadSize))

	encoded := h.bytes()

	for len(encoded)%8 != 0 {
		encoded = append(encoded, 0)
	}

	return encoded
}

func mainHeader(pkg Package, entries []Entry, built time.Time, payload []byte) ([]byte, error) {
	h := header{region: regionImmutable}

	h.addStrings(tagI18NTable, []string{"C"})
	h.addString(tagName, pkg.Name)
	h.addString(tagVersion, pkg.Version)
	h.addString(tagRelease, pkg.Release)
	h.addI18N(tagSummary, pkg.Summary)
	h.addI18N(tagDescription, pkg.Description)
	h.addInt32(tagBuildTime, uint32(built.Unix()))
	h.addString(tagBuildHost, "localhost")

	var installed uint32

	for _, entry := range entries {
		installed += uint32(len(entry.Data))
	}

	h.addInt32(tagSize, installed)

	if pkg.Vendor != "" {
		h.addString(tagVendor, pkg.Vendor)
	}

	h.addString(tagLicense, pkg.License)

	if pkg.Packager != "" {
		h.addString(tagPackager, pkg.Packager)
	}

	if pkg.URL != "" {
		h.addString(tagURL, pkg.URL)
	}

	h.addString(tagOS, "linux")
	h.addString(tagArch, pkg.Arch)
	h.addString(tagRPMVersion, "4.14.0")
	h.addString(tagPayloadFormat, "cpio")
	h.addString(tagPayloadCompress, "gzip")
	h.addString(tagPayloadFlags, "9")

	// What a current rpm checks the compressed payload against.
	payloadSum := sha256.Sum256(payload)
	h.addStrings(tagPayloadDigest, []string{hex.EncodeToString(payloadSum[:])})
	h.addInt32(tagPayloadDigestAl, 8)

	for _, script := range []struct {
		body, program uint32
		text          string
	}{
		{tagPreIn, tagPreInProg, pkg.PreIn},
		{tagPostIn, tagPostInProg, pkg.PostIn},
		{tagPreUn, tagPreUnProg, pkg.PreUn},
		{tagPostUn, tagPostUnProg, pkg.PostUn},
	} {
		if script.text != "" {
			h.addString(script.body, script.text)
			h.addString(script.program, "/bin/sh")
		}
	}

	h.addStrings(tagRequireName, []string{"/bin/sh", "rpmlib(CompressedFileNames)", "rpmlib(PayloadFilesHavePrefix)"})
	h.addInt32(tagRequireFlags, 0, 0x0100000A, 0x0100000A)
	h.addStrings(tagRequireVersion, []string{"", "3.0.4-1", "4.0-1"})

	h.addStrings(tagProvideName, []string{pkg.Name})
	h.addInt32(tagProvideFlags, 8)
	h.addStrings(tagProvideVersion, []string{pkg.Version + "-" + pkg.Release})

	addFiles(&h, entries, built)

	return h.bytes(), nil
}

// addFiles writes the file list: names split into directory and base name, and
// one value per file for modes, sizes, digests and the rest.
func addFiles(h *header, entries []Entry, built time.Time) {
	if len(entries) == 0 {
		return
	}

	var (
		dirNames   []string
		dirIndex   = map[string]uint32{}
		indexes    []uint32
		baseNames  []string
		modes      []uint16
		sizes      []uint32
		mtimes     []uint32
		digests    []string
		flags      []uint32
		users      []string
		groups     []string
		devices    []uint32
		inodes     []uint32
		rdevs      []uint16
		linkTos    []string
		languages  []string
		digestAlgo = uint32(8) // SHA-256
	)

	for number, entry := range entries {
		dir, base := path.Split(entry.Path)

		if _, known := dirIndex[dir]; !known {
			dirIndex[dir] = uint32(len(dirNames))
			dirNames = append(dirNames, dir)
		}

		indexes = append(indexes, dirIndex[dir])
		baseNames = append(baseNames, base)

		mode, size, digest := uint16(0o100000|entry.Mode), uint32(len(entry.Data)), ""

		if entry.Dir {
			mode, size = uint16(0o040000|entry.Mode), 4096
		} else {
			sum := sha256.Sum256(entry.Data)
			digest = hex.EncodeToString(sum[:])
		}

		var flag uint32

		if entry.Config {
			flag |= flagConfig | flagNoReplace
		}

		if entry.Doc {
			flag |= flagDoc
		}

		modes = append(modes, mode)
		sizes = append(sizes, size)
		mtimes = append(mtimes, uint32(built.Unix()))
		digests = append(digests, digest)
		flags = append(flags, flag)
		users = append(users, "root")
		groups = append(groups, "root")
		devices = append(devices, 1)
		inodes = append(inodes, uint32(number+1))
		rdevs = append(rdevs, 0)
		linkTos = append(linkTos, "")
		languages = append(languages, "")
	}

	h.addInt32(tagFileSizes, sizes...)
	h.addInt16(tagFileModes, modes...)
	h.addInt16(tagFileRdevs, rdevs...)
	h.addInt32(tagFileMtimes, mtimes...)
	h.addStrings(tagFileDigests, digests)
	h.addStrings(tagFileLinkTos, linkTos)
	h.addInt32(tagFileFlags, flags...)
	h.addStrings(tagFileUserName, users)
	h.addStrings(tagFileGroupName, groups)
	h.addInt32(tagFileDevices, devices...)
	h.addInt32(tagFileInodes, inodes...)
	h.addStrings(tagFileLangs, languages)
	h.addInt32(tagDirIndexes, indexes...)
	h.addStrings(tagBaseNames, baseNames)
	h.addStrings(tagDirNames, dirNames)
	h.addInt32(tagFileDigestAlgo, digestAlgo)
}

// readFile is a convenience for the command: the whole content of a file.
func readFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}
