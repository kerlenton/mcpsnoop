// Command npmpack builds the npm packages for one mcpsnoop release.
//
// It is a release tool, not part of the mcpsnoop command. It reads the archives
// a release publishes, checks them against the checksums file published beside
// them, and writes the seven package trees that carry those same bytes to npm.
//
//	npmpack -version 0.19.0 -dist dist -out dist/npm
//
// With -print-order it writes nothing and lists the directories in the order
// they have to be published in, one per line, and with -print-dist-tag it prints
// the npm tag the release belongs under, so the release workflow does not have
// to know either of those itself.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kerlenton/mcpsnoop/internal/npmpack"
)

func main() {
	var (
		version = flag.String("version", "", "the release being packaged, with or without a leading v")
		dist    = flag.String("dist", "dist", "directory holding the released archives and checksums.txt")
		source  = flag.String("source", filepath.Join("npm", "mcpsnoop"), "directory holding the checked-in root package")
		out     = flag.String("out", filepath.Join("dist", "npm"), "directory to write the package trees to")
		order   = flag.Bool("print-order", false, "write nothing and list the publish order instead")
		tag     = flag.Bool("print-dist-tag", false, "write nothing and print the npm dist-tag for -version instead")
	)
	flag.Parse()

	if *order {
		for _, dir := range npmpack.PublishOrder() {
			fmt.Println(dir)
		}
		return
	}
	if *tag {
		fmt.Println(npmpack.DistTag(*version))
		return
	}

	if err := npmpack.Build(npmpack.Options{
		Version:   *version,
		DistDir:   *dist,
		SourceDir: *source,
		OutDir:    *out,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
