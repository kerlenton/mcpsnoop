// Package npmpack turns a released set of mcpsnoop archives into the npm
// packages that carry them.
//
// The npm distribution is seven packages. Six hold one binary each and name the
// machine they are for through npm's os and cpu fields, so npm installs exactly
// one of them and skips the rest. The seventh is the thin package a user asks
// for by name; it holds a launcher and lists the other six as optional
// dependencies. That shape is what lets the install work behind a proxy, from a
// cache, and under --ignore-scripts, none of which hold for a postinstall that
// fetches from GitHub.
//
// The input is the published archives and their checksums file, not the build
// tree they came from. Two reasons. The archive is the artifact the project
// promises, while the layout of a build directory is a private detail of
// whatever produced it. And going through the checksums file means the bytes
// inside the npm package are provably the bytes of the GitHub Release, which is
// the claim worth being able to make about a binary strangers run with npx.
package npmpack

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options describes one run.
type Options struct {
	// Version is the release being packaged, without a leading v.
	Version string
	// DistDir holds the archives and the checksums file.
	DistDir string
	// SourceDir holds the checked-in root package: the launcher, its manifest,
	// the README and the licence.
	SourceDir string
	// OutDir receives the finished package trees. It is created if missing and
	// must be empty of anything this run would overwrite.
	OutDir string
}

// Target is one of the six machines a release is built for.
type Target struct {
	GOOS   string // as the Go toolchain and the archive name spell it
	GOARCH string
	OS     string // as npm's os field spells it
	CPU    string // as npm's cpu field spells it
}

// Package is the npm name of the platform package for t.
func (t Target) Package() string { return "@mcpsnoop/" + t.Dir() }

// Dir is the directory the platform package is written to, and the second half
// of its npm name.
func (t Target) Dir() string { return t.OS + "-" + t.CPU }

// Binary is the name of the executable inside the archive and the package.
func (t Target) Binary() string {
	if t.GOOS == "windows" {
		return "mcpsnoop.exe"
	}
	return "mcpsnoop"
}

// Archive is the file the release publishes for t.
func (t Target) Archive(version string) string {
	ext := "tar.gz"
	if t.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("mcpsnoop_%s_%s_%s.%s", version, t.GOOS, t.GOARCH, ext)
}

// Targets lists every machine the npm distribution covers, in the order the
// manifests should list them. It matches the goos and goarch matrix in
// .goreleaser.yaml, and the launcher's own table, and a change to one of the
// three without the other two is a bug the tests catch.
func Targets() []Target {
	return []Target{
		{GOOS: "darwin", GOARCH: "arm64", OS: "darwin", CPU: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64", OS: "darwin", CPU: "x64"},
		{GOOS: "linux", GOARCH: "arm64", OS: "linux", CPU: "arm64"},
		{GOOS: "linux", GOARCH: "amd64", OS: "linux", CPU: "x64"},
		{GOOS: "windows", GOARCH: "arm64", OS: "win32", CPU: "arm64"},
		{GOOS: "windows", GOARCH: "amd64", OS: "win32", CPU: "x64"},
	}
}

const (
	// checksumsFile is what the release calls its checksums file.
	checksumsFile = "checksums.txt"
	// placeholder is the version the checked-in root manifest carries. It is
	// never a real release, so a run that forgot to stamp it is caught rather
	// than published.
	placeholder = "0.0.0"
	// maxBinary bounds how much of an archive member is read. The binaries are
	// around 14 MB, so this leaves room to grow without letting a malformed or
	// hostile archive decompress without limit.
	maxBinary = 128 << 20
	// binaryMode is the mode the executable is written with. npm carries the
	// mode of a file in the tarball through to the installed tree, and a
	// binary that arrives without its execute bit cannot be run at all.
	binaryMode = 0o755
)

// Build writes the seven package trees for one release.
//
// Everything is verified before anything is written. A release that is missing
// one of its six archives, or that has one whose checksum does not match, must
// produce no packages at all: a root package published against platform
// packages that do not exist is broken for every user on that platform, and
// npm versions cannot be replaced once published.
func Build(opts Options) error {
	version := strings.TrimPrefix(opts.Version, "v")
	if version == "" {
		return fmt.Errorf("npmpack: no version given")
	}
	if version == placeholder {
		return fmt.Errorf("npmpack: %s is the placeholder version in the checked-in manifest, not a release", placeholder)
	}
	if strings.HasPrefix(version, "v") {
		return fmt.Errorf("npmpack: version %q still starts with a v", version)
	}

	sums, err := readChecksums(filepath.Join(opts.DistDir, checksumsFile))
	if err != nil {
		return err
	}

	binaries := make(map[string][]byte, len(Targets()))
	for _, t := range Targets() {
		bin, err := verifiedBinary(opts.DistDir, t, version, sums)
		if err != nil {
			return err
		}
		binaries[t.Dir()] = bin
	}

	// Each platform package ships a binary, so each one carries the licence
	// that binary is under rather than pointing at a sibling package for it.
	licence, err := os.ReadFile(filepath.Join(opts.SourceDir, "LICENSE"))
	if err != nil {
		return fmt.Errorf("npmpack: %w", err)
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return err
	}
	for _, t := range Targets() {
		if err := writePlatform(opts.OutDir, t, version, binaries[t.Dir()], licence); err != nil {
			return err
		}
	}
	return writeRoot(opts.SourceDir, opts.OutDir, version)
}

// verifiedBinary reads t's archive, checks it against the checksums file, and
// returns the executable inside it.
func verifiedBinary(dist string, t Target, version string, sums map[string]string) ([]byte, error) {
	name := t.Archive(version)
	want, ok := sums[name]
	if !ok {
		return nil, fmt.Errorf("npmpack: %s does not list %s, so its contents cannot be verified", checksumsFile, name)
	}
	raw, err := os.ReadFile(filepath.Join(dist, name))
	if err != nil {
		return nil, fmt.Errorf("npmpack: %w", err)
	}
	got := sha256.Sum256(raw)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("npmpack: %s does not match its checksum in %s, so it is not the released archive", name, checksumsFile)
	}
	bin, err := extract(raw, t)
	if err != nil {
		return nil, fmt.Errorf("npmpack: %s: %w", name, err)
	}
	if len(bin) == 0 {
		return nil, fmt.Errorf("npmpack: %s holds an empty %s", name, t.Binary())
	}
	return bin, nil
}

// extract pulls the executable out of one archive.
func extract(raw []byte, t Target) ([]byte, error) {
	if t.GOOS == "windows" {
		return extractZip(raw, t.Binary())
	}
	return extractTarGz(raw, t.Binary())
}

func extractTarGz(raw []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != name {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxBinary))
	}
	return nil, fmt.Errorf("holds no %s", name)
}

func extractZip(raw []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxBinary))
	}
	return nil, fmt.Errorf("holds no %s", name)
}

// readChecksums parses the released checksums file into name to digest.
func readChecksums(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("npmpack: %w", err)
	}
	defer f.Close()

	sums := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		// The name may carry the binary marker some tools write.
		sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("npmpack: %w", err)
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("npmpack: %s lists nothing", path)
	}
	return sums, nil
}

// platformManifest is one platform package's package.json.
//
// It deliberately declares no bin. npm would install a shim for every bin it
// finds, and six packages all claiming the name mcpsnoop would fight the root
// package for it. The root resolves the path itself instead.
type platformManifest struct {
	Name            string     `json:"name"`
	Version         string     `json:"version"`
	Description     string     `json:"description"`
	Homepage        string     `json:"homepage"`
	Repository      repository `json:"repository"`
	License         string     `json:"license"`
	OS              []string   `json:"os"`
	CPU             []string   `json:"cpu"`
	Files           []string   `json:"files"`
	Engines         engines    `json:"engines"`
	PreferUnplugged bool       `json:"preferUnplugged"`
}

type repository struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Directory string `json:"directory,omitempty"`
}

type engines struct {
	Node string `json:"node"`
}

func writePlatform(out string, t Target, version string, bin, licence []byte) error {
	dir := filepath.Join(out, "@mcpsnoop", t.Dir())
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", t.Binary()), bin, binaryMode); err != nil {
		return err
	}
	// WriteFile leaves an existing file's mode alone, and the execute bit is
	// the difference between a working install and one that cannot start.
	if err := os.Chmod(filepath.Join(dir, "bin", t.Binary()), binaryMode); err != nil {
		return err
	}

	m := platformManifest{
		Name:        t.Package(),
		Version:     version,
		Description: fmt.Sprintf("The %s binary for mcpsnoop. Installed for you by the mcpsnoop package; not meant to be depended on directly.", t.Dir()),
		Homepage:    "https://github.com/kerlenton/mcpsnoop#readme",
		Repository: repository{
			Type: "git",
			URL:  "git+https://github.com/kerlenton/mcpsnoop.git",
		},
		License:         "MIT",
		OS:              []string{t.OS},
		CPU:             []string{t.CPU},
		Files:           []string{"bin/" + t.Binary()},
		Engines:         engines{Node: ">=18"},
		PreferUnplugged: true,
	}
	if err := writeJSON(filepath.Join(dir, "package.json"), m); err != nil {
		return err
	}

	readme := fmt.Sprintf(`# %s

The %s build of [mcpsnoop](https://github.com/kerlenton/mcpsnoop).

This package holds one binary and nothing else. You do not install it yourself.
Install `+"`mcpsnoop`"+`, and npm picks whichever of these six packages matches your
machine and skips the other five.

    npx mcpsnoop -- node build/index.js

MIT licensed.
`, t.Package(), t.Dir())
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "LICENSE"), licence, 0o644)
}

// writeRoot copies the checked-in root package and stamps the release version
// into its manifest, pinning every platform package to the same version.
func writeRoot(src, out, version string) error {
	dir := filepath.Join(out, "mcpsnoop")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	for _, name := range []string{"README.md", "LICENSE", filepath.Join("bin", "mcpsnoop.js")} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return fmt.Errorf("npmpack: %w", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}

	raw, err := os.ReadFile(filepath.Join(src, "package.json"))
	if err != nil {
		return fmt.Errorf("npmpack: %w", err)
	}
	// Decoding into a map keeps every field the checked-in manifest carries,
	// including ones added later that this code has never heard of, so the two
	// files cannot drift apart in the one direction that matters.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("npmpack: %s: %w", filepath.Join(src, "package.json"), err)
	}
	deps, ok := m["optionalDependencies"].(map[string]any)
	if !ok {
		return fmt.Errorf("npmpack: the root manifest lists no optionalDependencies, so nothing would carry the binary")
	}
	want := map[string]bool{}
	for _, t := range Targets() {
		want[t.Package()] = true
	}
	for name := range deps {
		if !want[name] {
			return fmt.Errorf("npmpack: the root manifest depends on %s, which is not one of the six platform packages", name)
		}
	}
	for name := range want {
		if _, ok := deps[name]; !ok {
			return fmt.Errorf("npmpack: the root manifest is missing %s, so that platform would install with no binary", name)
		}
		deps[name] = version
	}
	m["version"] = version
	return writeJSON(filepath.Join(dir, "package.json"), m)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// DistTag is the npm tag a release should be published under.
//
// npm serves whatever carries the latest tag to a bare `npx mcpsnoop`, and a
// publish claims that tag unless it is told otherwise. So a prerelease published
// without this would be handed to everyone, and there is no taking it back: npm
// does not let a version be replaced, and unpublishing closes after 72 hours.
//
// A prerelease is a version with a hyphen in it, which is what SemVer says and
// what the release tags spell.
func DistTag(version string) string {
	v := strings.TrimPrefix(version, "v")
	if strings.Contains(v, "-") {
		return "next"
	}
	return "latest"
}

// PublishOrder lists the seven package directories a run produces, relative to
// the output directory, in the order they must be published in.
//
// The root package resolves to the other six, so it goes last. A run that
// stopped halfway then leaves platform packages that nothing yet points at,
// which is inert, rather than a root package pointing at versions that do not
// exist, which is broken for everyone and cannot be taken back: npm does not
// let a published version be replaced.
func PublishOrder() []string {
	dirs := make([]string, 0, len(Targets())+1)
	for _, t := range Targets() {
		dirs = append(dirs, filepath.Join("@mcpsnoop", t.Dir()))
	}
	sort.Strings(dirs)
	return append(dirs, "mcpsnoop")
}
