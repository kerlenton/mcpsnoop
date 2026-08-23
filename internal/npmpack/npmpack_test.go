package npmpack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const testVersion = "0.19.0"

// release writes a plausible set of release archives and their checksums file
// into a fresh directory, and returns it.
func release(t *testing.T, mutate ...func(files map[string][]byte)) string {
	t.Helper()
	files := map[string][]byte{}
	for _, tgt := range Targets() {
		files[tgt.Archive(testVersion)] = archive(t, tgt, []byte("binary for "+tgt.Dir()))
	}
	for _, m := range mutate {
		m(files)
	}

	dir := t.TempDir()
	var sums []string
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		d := sha256.Sum256(body)
		sums = append(sums, fmt.Sprintf("%s  %s", hex.EncodeToString(d[:]), name))
	}
	sort.Strings(sums)
	if err := os.WriteFile(filepath.Join(dir, checksumsFile), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// archive packs one binary the way the release does for that platform.
func archive(t *testing.T, tgt Target, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if tgt.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		// The real archive carries these beside the binary, and the extractor
		// has to walk past them rather than take the first member.
		for _, name := range []string{"LICENSE", "README.md", tgt.Binary()} {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte("not the binary")
			if name == tgt.Binary() {
				body = bin
			}
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"LICENSE", "README.md", tgt.Binary()} {
		body := []byte("not the binary")
		mode := int64(0o644)
		if name == tgt.Binary() {
			body, mode = bin, 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// source copies the checked-in root package, so the tests run against the real
// launcher and the real manifest rather than a stand-in that could agree with
// the code while the shipped files did not.
func source(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "npm", "mcpsnoop")
}

func build(t *testing.T, dist, out string) error {
	t.Helper()
	return Build(Options{Version: testVersion, DistDir: dist, SourceDir: source(t), OutDir: out})
}

func readManifest(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not valid JSON, so npm would refuse the package: %v", path, err)
	}
	return m
}

func TestBuildWritesOnePackagePerPlatformAndTheRoot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "npm")
	if err := build(t, release(t), out); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range Targets() {
		m := readManifest(t, filepath.Join(out, "@mcpsnoop", tgt.Dir(), "package.json"))
		if m["name"] != tgt.Package() {
			t.Errorf("%s is named %v", tgt.Dir(), m["name"])
		}
		if m["version"] != testVersion {
			t.Errorf("%s is version %v, want %s", tgt.Dir(), m["version"], testVersion)
		}
		// Without these npm has no way to tell which of the six belongs on the
		// machine, and would install all of them.
		if got := m["os"].([]any); len(got) != 1 || got[0] != tgt.OS {
			t.Errorf("%s declares os %v, want [%s]", tgt.Dir(), got, tgt.OS)
		}
		if got := m["cpu"].([]any); len(got) != 1 || got[0] != tgt.CPU {
			t.Errorf("%s declares cpu %v, want [%s]", tgt.Dir(), got, tgt.CPU)
		}
		if _, ok := m["bin"]; ok {
			t.Errorf("%s declares a bin, which would fight the root package for the mcpsnoop name", tgt.Dir())
		}
	}
	root := readManifest(t, filepath.Join(out, "mcpsnoop", "package.json"))
	if root["version"] != testVersion {
		t.Errorf("the root package is version %v, want %s", root["version"], testVersion)
	}
}

func TestBuildPutsTheRightBinaryInEachPackage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "npm")
	if err := build(t, release(t), out); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range Targets() {
		path := filepath.Join(out, "@mcpsnoop", tgt.Dir(), "bin", tgt.Binary())
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// A run that shuffled the archives would still produce seven packages
		// that install cleanly, and would hand every user the wrong machine's
		// binary.
		if want := "binary for " + tgt.Dir(); string(got) != want {
			t.Errorf("%s holds %q, want %q", path, got, want)
		}
	}
}

func TestBuildLeavesTheBinaryExecutable(t *testing.T) {
	out := filepath.Join(t.TempDir(), "npm")
	if err := build(t, release(t), out); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range Targets() {
		if tgt.GOOS == "windows" {
			continue
		}
		fi, err := os.Stat(filepath.Join(out, "@mcpsnoop", tgt.Dir(), "bin", tgt.Binary()))
		if err != nil {
			t.Fatal(err)
		}
		// npm carries the mode through into the installed tree, so a binary
		// packed without this cannot be started at all.
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is packed as %v, which cannot be executed", tgt.Dir(), fi.Mode().Perm())
		}
	}
}

func TestBuildPinsEveryPlatformPackageToTheReleaseVersion(t *testing.T) {
	out := filepath.Join(t.TempDir(), "npm")
	if err := build(t, release(t), out); err != nil {
		t.Fatal(err)
	}
	deps := readManifest(t, filepath.Join(out, "mcpsnoop", "package.json"))["optionalDependencies"].(map[string]any)
	if len(deps) != len(Targets()) {
		t.Fatalf("the root package lists %d platform packages, want %d", len(deps), len(Targets()))
	}
	for _, tgt := range Targets() {
		// A range rather than an exact version would let npm pair a root
		// package with a binary from another release.
		if got := deps[tgt.Package()]; got != testVersion {
			t.Errorf("the root package wants %s at %v, want exactly %s", tgt.Package(), got, testVersion)
		}
	}
}

func TestBuildKeepsFieldsItDoesNotKnowAbout(t *testing.T) {
	src := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(source(t), "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["fundingLater"] = "https://example.invalid/funding"
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(src, "package.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README.md", "LICENSE"} {
		copyInto(t, filepath.Join(source(t), name), filepath.Join(src, name))
	}
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyInto(t, filepath.Join(source(t), "bin", "mcpsnoop.js"), filepath.Join(src, "bin", "mcpsnoop.js"))

	out := filepath.Join(t.TempDir(), "npm")
	if err := Build(Options{Version: testVersion, DistDir: release(t), SourceDir: src, OutDir: out}); err != nil {
		t.Fatal(err)
	}
	// A manifest rebuilt from a fixed struct would quietly drop anything added
	// to the checked-in one later, and the loss would only show on npm.
	if got := readManifest(t, filepath.Join(out, "mcpsnoop", "package.json"))["fundingLater"]; got != "https://example.invalid/funding" {
		t.Errorf("the stamped manifest dropped a field it did not recognise, got %v", got)
	}
}

func copyInto(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRefusesAnArchiveThatDoesNotMatchItsChecksum(t *testing.T) {
	dist := release(t, func(files map[string][]byte) {
		files["mcpsnoop_"+testVersion+"_linux_amd64.tar.gz"] = archive(t, Target{GOOS: "linux", GOARCH: "amd64", OS: "linux", CPU: "x64"}, []byte("swapped"))
	})
	// Rewrite that one archive on disk after the checksums were taken.
	tgt := Target{GOOS: "linux", GOARCH: "amd64", OS: "linux", CPU: "x64"}
	if err := os.WriteFile(filepath.Join(dist, tgt.Archive(testVersion)), archive(t, tgt, []byte("tampered")), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "npm")
	err := build(t, dist, out)
	if err == nil {
		t.Fatal("packaged an archive whose bytes are not the released ones")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("wrote packages anyway, so a partial publish is possible")
	}
}

func TestBuildRefusesAReleaseMissingOnePlatform(t *testing.T) {
	dist := release(t)
	missing := Target{GOOS: "windows", GOARCH: "arm64", OS: "win32", CPU: "arm64"}
	if err := os.Remove(filepath.Join(dist, missing.Archive(testVersion))); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "npm")
	// Publishing the other five plus a root package that names all six leaves
	// every win32-arm64 user with an install that resolves to nothing, and npm
	// versions cannot be replaced once published.
	if err := build(t, dist, out); err == nil {
		t.Fatal("packaged a release that is missing win32-arm64")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("wrote packages anyway")
	}
}

func TestBuildRefusesAnArchiveTheChecksumsFileDoesNotList(t *testing.T) {
	dist := release(t)
	raw, err := os.ReadFile(filepath.Join(dist, checksumsFile))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.Contains(line, "darwin_arm64") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(filepath.Join(dist, checksumsFile), []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := build(t, dist, filepath.Join(t.TempDir(), "npm")); err == nil {
		t.Fatal("packaged an archive that the checksums file does not vouch for")
	}
}

func TestBuildRefusesTheePlaceholderVersion(t *testing.T) {
	// The checked-in manifest carries 0.0.0 so that it is obvious it is a
	// template. Stamping it back in would publish a package that outranks
	// nothing and shadows every real release in a lockfile.
	err := Build(Options{Version: placeholder, DistDir: release(t), SourceDir: source(t), OutDir: filepath.Join(t.TempDir(), "npm")})
	if err == nil {
		t.Fatal("packaged the placeholder version as if it were a release")
	}
	if !strings.Contains(err.Error(), placeholder) {
		t.Errorf("the error does not name the version: %v", err)
	}
}

func TestBuildTakesATagWithItsLeadingV(t *testing.T) {
	out := filepath.Join(t.TempDir(), "npm")
	// The release is driven by a pushed tag, which is spelled v0.19.0, while
	// npm versions and the archive names are not.
	if err := Build(Options{Version: "v" + testVersion, DistDir: release(t), SourceDir: source(t), OutDir: out}); err != nil {
		t.Fatal(err)
	}
	if got := readManifest(t, filepath.Join(out, "mcpsnoop", "package.json"))["version"]; got != testVersion {
		t.Errorf("the root package is version %v, want %s", got, testVersion)
	}
}

func TestBuildRefusesARootManifestMissingAPlatform(t *testing.T) {
	src := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(source(t), "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m["optionalDependencies"].(map[string]any), "@mcpsnoop/linux-x64")
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(src, "package.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README.md", "LICENSE"} {
		copyInto(t, filepath.Join(source(t), name), filepath.Join(src, name))
	}
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyInto(t, filepath.Join(source(t), "bin", "mcpsnoop.js"), filepath.Join(src, "bin", "mcpsnoop.js"))

	err = Build(Options{Version: testVersion, DistDir: release(t), SourceDir: src, OutDir: filepath.Join(t.TempDir(), "npm")})
	if err == nil {
		t.Fatal("packaged a root manifest that leaves linux-x64 with no binary")
	}
	if !strings.Contains(err.Error(), "@mcpsnoop/linux-x64") {
		t.Errorf("the error does not name the missing platform: %v", err)
	}
}

func TestBuildRefusesAnArchiveWithNoBinaryInIt(t *testing.T) {
	tgt := Target{GOOS: "linux", GOARCH: "amd64", OS: "linux", CPU: "x64"}
	dist := release(t, func(files map[string][]byte) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := []byte("only docs here")
		if err := tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()
		files[tgt.Archive(testVersion)] = buf.Bytes()
	})
	err := build(t, dist, filepath.Join(t.TempDir(), "npm"))
	if err == nil {
		t.Fatal("packaged an archive that holds no binary")
	}
	if !strings.Contains(err.Error(), "mcpsnoop") {
		t.Errorf("the error does not say what was missing: %v", err)
	}
}

func TestPublishOrderPutsTheRootPackageLast(t *testing.T) {
	order := PublishOrder()
	if len(order) != len(Targets())+1 {
		t.Fatalf("the order lists %d packages, want %d", len(order), len(Targets())+1)
	}
	// The root package resolves to the other six. Published first, it is broken
	// for the window before they land, and npm serves it to anyone installing
	// in that window.
	if got := order[len(order)-1]; got != "mcpsnoop" {
		t.Errorf("the last package published is %s, want mcpsnoop", got)
	}
	for _, name := range order[:len(order)-1] {
		if !strings.HasPrefix(name, "@mcpsnoop/") {
			t.Errorf("%s is published before the root package but is not a platform package", name)
		}
	}
}

func TestPublishOrderNamesADirectoryThatBuildWrote(t *testing.T) {
	out := filepath.Join(t.TempDir(), "npm")
	if err := build(t, release(t), out); err != nil {
		t.Fatal(err)
	}
	for _, name := range PublishOrder() {
		// The workflow asks the registry about a name and then publishes the
		// directory of that same name, so the two have to be the one string.
		// A name with nothing behind it is a release that fails halfway.
		dir := filepath.Join(out, filepath.FromSlash(name))
		if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
			t.Errorf("the publish order names %s, which was not written: %v", name, err)
		}
		if got := readManifest(t, filepath.Join(dir, "package.json"))["name"]; got != name {
			t.Errorf("the directory %s holds the package %v, so the workflow would ask the registry about the wrong name", name, got)
		}
	}
}

func TestTargetsMatchTheReleaseMatrix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// A platform added to the release but not here would be built and published
	// to GitHub and silently left out of npm.
	want := map[string]bool{}
	for _, goos := range yamlList(t, raw, "goos") {
		for _, goarch := range yamlList(t, raw, "goarch") {
			want[goos+"/"+goarch] = true
		}
	}
	got := map[string]bool{}
	for _, tgt := range Targets() {
		got[tgt.GOOS+"/"+tgt.GOARCH] = true
	}
	for pair := range want {
		if !got[pair] {
			t.Errorf(".goreleaser.yaml builds %s but the npm packages do not cover it", pair)
		}
	}
	for pair := range got {
		if !want[pair] {
			t.Errorf("the npm packages cover %s but .goreleaser.yaml does not build it", pair)
		}
	}
}

// yamlList reads one flow-style list from .goreleaser.yaml. It is deliberately
// narrow, and fails loudly rather than returning nothing, so that a rewrite of
// the release config is noticed here instead of quietly disarming the check.
func yamlList(t *testing.T, raw []byte, key string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + key + `:\s*\[([^\]]*)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf(".goreleaser.yaml no longer spells %s as a flow list, so this check reads nothing; update it", key)
	}
	var out []string
	for _, f := range strings.Split(string(m[1]), ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatalf(".goreleaser.yaml lists no %s", key)
	}
	return out
}

func TestTargetsMatchTheLauncherTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(source(t), "bin", "mcpsnoop.js"))
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`'(@mcpsnoop/[a-z0-9-]+)'`).FindAllSubmatch(raw, -1)
	if len(found) == 0 {
		t.Fatal("the launcher no longer names its platform packages the way this check reads them; update it")
	}
	got := map[string]bool{}
	for _, m := range found {
		got[string(m[1])] = true
	}
	want := map[string]bool{}
	for _, tgt := range Targets() {
		want[tgt.Package()] = true
	}
	// The launcher decides at runtime which package to look for. A platform
	// packaged here but absent there installs fine and then refuses to start.
	for name := range want {
		if !got[name] {
			t.Errorf("%s is packaged but the launcher never looks for it", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("the launcher looks for %s, which is never packaged", name)
		}
	}
}

func TestTargetsMatchTheRootManifest(t *testing.T) {
	deps, ok := readManifest(t, filepath.Join(source(t), "package.json"))["optionalDependencies"].(map[string]any)
	if !ok {
		t.Fatal("the checked-in root manifest lists no optionalDependencies")
	}
	for _, tgt := range Targets() {
		if _, ok := deps[tgt.Package()]; !ok {
			t.Errorf("the checked-in root manifest does not depend on %s", tgt.Package())
		}
	}
	if len(deps) != len(Targets()) {
		t.Errorf("the checked-in root manifest lists %d platform packages, want %d", len(deps), len(Targets()))
	}
}

func TestArchiveNamesMatchTheReleaseTemplate(t *testing.T) {
	// .goreleaser.yaml names archives {{ .ProjectName }}_{{ .Version }}_{{ .Os
	// }}_{{ .Arch }}, gzipped except on windows. Reading the wrong name means
	// every archive looks missing.
	for _, tgt := range Targets() {
		want := "mcpsnoop_" + testVersion + "_" + tgt.GOOS + "_" + tgt.GOARCH
		if tgt.GOOS == "windows" {
			want += ".zip"
		} else {
			want += ".tar.gz"
		}
		if got := tgt.Archive(testVersion); got != want {
			t.Errorf("expects %s, the release publishes %s", got, want)
		}
	}
}

func TestDistTagKeepsAPrereleaseOffLatest(t *testing.T) {
	for _, c := range []struct{ version, want string }{
		{"0.19.0", "latest"},
		{"v0.19.0", "latest"},
		{"1.0.0", "latest"},
		// npm hands whatever carries latest to a bare npx mcpsnoop. A release
		// candidate published there reaches everyone, and the version cannot
		// be replaced afterwards.
		{"0.20.0-rc.1", "next"},
		{"v0.20.0-rc.1", "next"},
		{"1.0.0-beta.3", "next"},
		{"0.19.0-next.20260823", "next"},
	} {
		if got := DistTag(c.version); got != c.want {
			t.Errorf("DistTag(%q) = %q, want %q", c.version, got, c.want)
		}
	}
}

func TestTheNpmLicenceIsTheProjectLicence(t *testing.T) {
	// npm wants a licence file inside the package, so the root package carries
	// its own copy and the six platform packages get that copy stamped into
	// them. Two files that must stay the same word for word, and only one of
	// them is the one anybody edits.
	repo, err := os.ReadFile(filepath.Join("..", "..", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(source(t), "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repo, shipped) {
		t.Error("npm/mcpsnoop/LICENSE has drifted from LICENSE, so npm would ship different terms from the repository")
	}
}
