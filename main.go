package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	zglob "github.com/mattn/go-zglob"
)

var (
	flags               = flag.NewFlagSet("modvendor", flag.ExitOnError)
	copyPatFlag         = flags.String("copy", "", "copy files matching glob pattern to ./vendor/, e.g., modvendor -copy=\"**/*.c **/*.h **/*.proto\"")
	includePackagesFlag = flags.String("include", "", "additional packages untracked in vendor/modules.txt, e.g., modvendor -include=\"github.com/tensorflow/tensorflow/tensorflow/c\"")
	verboseFlag         = flags.Bool("v", false, "verbose output")
)

type Mod struct {
	ImportPath    string
	SourcePath    string
	Version       string
	SourceVersion string
	Dir           string          // full path, $GOPATH/pkg/mod/
	Pkgs          []string        // sub-pkg import paths
	VendorList    map[string]bool // files to vendor
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	must(flags.Parse(os.Args[1:]))

	// Ensure go.mod file exists and we're running from the project root,
	// and that ./vendor/modules.txt file exists.
	cwd, err := os.Getwd()
	must(err)

	// Ensure that a go.mod exists in the project.
	modPath := filepath.Join(cwd, "go.mod")
	if _, err := os.Stat(modPath); os.IsNotExist(err) {
		must(fmt.Errorf("%s not found. Run `go mod vendor` and try again.", modPath))
	}

	// Ensure that a vendor/modules.txt exists in the project.
	modtxtPath := filepath.Join(cwd, "vendor", "modules.txt")
	if _, err := os.Stat(modtxtPath); os.IsNotExist(err) {
		must(fmt.Errorf("%s not found. Run `go mod vendor` and try again.", modtxtPath))
	}

	// Prepare vendor copy patterns.
	copyPat := strings.Split(strings.TrimSpace(*copyPatFlag), " ")
	if len(copyPat) == 0 {
		must(errors.New("-copy argument is empty."))
	}

	// Prepare additional packages.
	includedPackages := strings.Split(strings.TrimSpace(*includePackagesFlag), " ")

	// Parse/process modules.txt file of pkgs.
	f, _ := os.Open(modtxtPath)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)

	var mod *Mod
	modules := []*Mod{}

	for scanner.Scan() {
		line := scanner.Text()

		if line[0] == 35 { // i.e., #
			s := strings.Split(line, " ")

			mod = &Mod{
				ImportPath: s[1],
				Version:    s[2],
			}
			if s[2] == "=>" {
				// issue https://github.com/golang/go/issues/33848 added these,
				// see comments. I think we can get away with ignoring them.
				continue
			}
			// Handle "replace" in module file if any
			if len(s) > 3 && s[3] == "=>" {
				mod.SourcePath = s[4]
				mod.SourceVersion = s[5]
				mod.Dir = pkgModPath(mod.SourcePath, mod.SourceVersion)
			} else {
				mod.Dir = pkgModPath(mod.ImportPath, mod.Version)
			}

			if _, err := os.Stat(mod.Dir); os.IsNotExist(err) {
				must(fmt.Errorf("%s module path does not exist. Check $GOPATH/pkg/mod.", mod.Dir))
			}

			// Build list of files to module path source to
			// project vendor folder.
			mod.VendorList = buildModVendorList(copyPat, mod)
			modules = append(modules, mod)

			continue
		}

		mod.Pkgs = append(mod.Pkgs, line)
	}

	// Filter out files not part of the mod.Pkgs.
	for _, mod := range modules {
		if len(mod.VendorList) == 0 {
			continue
		}

		for _, pkg := range includedPackages {
			if strings.HasPrefix(pkg, mod.ImportPath) {
				mod.Pkgs = append(mod.Pkgs, pkg)
			}
		}

		for vendorFile := range mod.VendorList {
			for _, subpkg := range mod.Pkgs {
				path := filepath.Join(mod.Dir, importPathIntersect(mod.ImportPath, subpkg))

				x := strings.Index(vendorFile, path)
				if x == 0 {
					mod.VendorList[vendorFile] = true
				}
			}
		}
		for vendorFile, toggle := range mod.VendorList {
			if !toggle {
				delete(mod.VendorList, vendorFile)
			}
		}
	}

	// Copy mod vendor list files to ./vendor.
	for _, mod := range modules {
		for vendorFile := range mod.VendorList {
			x := strings.Index(vendorFile, mod.Dir)
			if x < 0 {
				fmt.Println("Error! vendor file doesn't belong to mod, strange.")
				os.Exit(1)
			}

			localPath := fmt.Sprintf("%s%s", mod.ImportPath, vendorFile[len(mod.Dir):])
			localFile := fmt.Sprintf("./vendor/%s", localPath)

			if *verboseFlag {
				fmt.Printf("vendoring %s\n", localPath)
			}

			must(os.MkdirAll(filepath.Dir(localFile), os.ModePerm))
			if _, err := copyFile(vendorFile, localFile); err != nil {
				fmt.Printf("Error! %s - unable to copy file %s\n", err.Error(), vendorFile)
				os.Exit(1)
			}
		}
	}
}

func buildModVendorList(copyPat []string, mod *Mod) map[string]bool {
	vendorList := map[string]bool{}

	for _, pat := range copyPat {
		matches, err := zglob.Glob(filepath.Join(mod.Dir, pat))
		if err != nil {
			must(fmt.Errorf("glob match failure: %v", err))
		}

		for _, m := range matches {
			vendorList[m] = false
		}
	}

	return vendorList
}

func importPathIntersect(basePath, pkgPath string) string {
	if strings.Index(pkgPath, basePath) != 0 {
		return ""
	}
	return pkgPath[len(basePath):]
}

func pkgModPath(importPath, version string) string {
	goPath := os.Getenv("GOPATH")
	if goPath == "" {
		// the default GOPATH for go v1.11
		goPath = filepath.Join(os.Getenv("HOME"), "go")
	}

	var normPath string

	for _, char := range importPath {
		if unicode.IsUpper(char) {
			normPath += "!" + string(unicode.ToLower(char))
		} else {
			normPath += string(char)
		}
	}

	return filepath.Join(goPath, "pkg", "mod", fmt.Sprintf("%s@%s", normPath, version))
}

func copyFile(src, dst string) (int64, error) {
	srcStat, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	if !srcStat.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", src)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer dstFile.Close()

	return io.Copy(dstFile, srcFile)
}
