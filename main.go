package main

import (
	"bufio"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

// TODO:
// default to current dir
// warn if a pkg is belongs to an existing submodule, but it wasn't found there
// same, but for recursive submodules
// prune support
// update support
// explicit project name
// proper go-import meta tag handling
// check that declared package names match dirs
// infer project name from GOPATH
// use type aliases for packages and paths?

func main() {
	rootDir := "."
	if len(os.Args) > 1 {
		rootDir = os.Args[1]
	}

	if err := run(rootDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type vendetta struct {
	rootDir string
	goPath
	goPaths       map[string]*goPath
	processedDirs map[string]struct{}
	trace         []trace
}

type trace struct {
	dir string
	pkg string
}

// Conceptually, a gopath element.  These are arranged into linked
// lists, representing the gopath applicable for a particular
// directory (produced by getGoPath and memoized in the goPaths map).
type goPath struct {
	dir      string
	prefixes map[string]struct{}
	next     *goPath
}

func run(rootDir string) error {
	v := vendetta{
		rootDir:       rootDir,
		goPaths:       make(map[string]*goPath),
		processedDirs: make(map[string]struct{}),
	}

	v.goPaths[""] = &goPath{dir: "vendor", next: &v.goPath}
	v.prefixes = make(map[string]struct{})
	v.inferProjectNameFromGit()

	if len(v.prefixes) == 0 {
		return fmt.Errorf("Unable to infer project name")
	}

	return v.processRecursive("", true)
}

var remoteUrlRE = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)(.*\.?)$`)

func (v *vendetta) inferProjectNameFromGit() error {
	remotes, err := popen("git", "-C", v.rootDir, "remote", "-v")
	if err != nil {
		return err
	}

	defer remotes.close()

	for remotes.Scan() {
		fields := splitWS(remotes.Text())
		m := remoteUrlRE.FindStringSubmatch(fields[1])
		if m != nil {
			name := m[1]
			if strings.HasSuffix(name, ".git") {
				name = name[:len(name)-4]
			}

			name = "github.com/" + name
			if _, found := v.prefixes[name]; !found {
				fmt.Println("Inferred package name", name, "from git remote")
				v.prefixes[name] = struct{}{}
			}
		}
	}

	if err := remotes.close(); err != nil {
		return err
	}

	return nil
}

var wsRE = regexp.MustCompile(`[ \t]+`)

func splitWS(s string) []string {
	return wsRE.Split(s, -1)
}

func (v *vendetta) submoduleAdd(url, dir string) error {
	out, err := exec.Command("git", "-C", v.rootDir, "submodule", "add",
		url, dir).CombinedOutput()
	if err != nil {
		os.Stderr.Write(out)
	}

	return err
}

type popenLines struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	*bufio.Scanner
}

func popen(name string, args ...string) (popenLines, error) {
	cmd := exec.Command(name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return popenLines{}, err
	}

	cmd.Stderr = os.Stderr
	p := popenLines{cmd: cmd, stdout: stdout}

	if err := cmd.Start(); err != nil {
		return popenLines{}, err
	}

	p.Scanner = bufio.NewScanner(stdout)
	return p, nil
}

func (p popenLines) close() error {
	res := p.Scanner.Err()
	setRes := func(err error) {
		if res == nil {
			res = err
		}
	}

	if p.stdout != nil {
		_, err := io.Copy(ioutil.Discard, p.stdout)
		p.stdout = nil
		if err != nil {
			setRes(err)
			p.cmd.Process.Kill()
		}
	}

	if p.cmd != nil {
		setRes(p.cmd.Wait())
		p.cmd = nil
	}

	return res
}

func (v *vendetta) realDir(dir string) string {
	return path.Join(v.rootDir, dir)
}

func (v *vendetta) processRecursive(dir string, root bool) error {
	if err := v.process(dir, true, false); err != nil {
		return err
	}

	var subdirs []string
	if err := readDir(v.realDir(dir), func(fi os.FileInfo) bool {
		if fi.IsDir() {
			subdirs = append(subdirs, fi.Name())
		}
		return true
	}); err != nil {
		return err
	}

	for _, subdir := range subdirs {
		switch subdir {
		case "vendor":
			if root {
				continue
			}
		case "testdata":
			continue
		}

		err := v.processRecursive(path.Join(dir, subdir), false)
		if err != nil {
			return err
		}
	}

	return nil
}

func (v *vendetta) process(dir string, testsToo bool, strict bool) error {
	if _, found := v.processedDirs[dir]; found {
		return nil
	}

	v.processedDirs[dir] = struct{}{}

	pkg, err := build.Default.ImportDir(v.realDir(dir), 0)
	if err != nil {
		if _, ok := err.(*build.NoGoError); ok && !strict {
			return nil
		}

		return fmt.Errorf("gathering imports in %s: %s",
			v.realDir(dir), err)
	}

	deps := func(imports []string) error {
		for _, imp := range imports {
			v.trace = append(v.trace, trace{dir, imp})

			if err := v.dependency(dir, imp); err != nil {
				return err
			}

			v.trace = v.trace[:len(v.trace)-1]
		}
		return nil
	}

	if err := deps(pkg.Imports); err != nil {
		return err
	}

	if testsToo {
		if err := deps(pkg.TestImports); err != nil {
			return err
		}
	}

	return nil
}

func (v *vendetta) dependency(dir string, pkg string) error {
	// Search the gopath for an existing package
	gp, err := v.getGoPath(dir)
	if err != nil {
		return err
	}

	for gp != nil {
		found, pkgdir, err := gp.provides(pkg, v)
		if err != nil {
			return err
		}

		if found {
			return v.process(pkgdir, false, true)
		}

		gp = gp.next
	}

	// Figure out how to obtain the package. This is a rough
	// approximation of what golang's vcs.go does:
	bits := strings.Split(pkg, "/")

	// Exclude golang standard packages
	if !strings.Contains(bits[0], ".") {
		return nil
	}

	// There's really no match for this package.  We need
	// to add it.
	f := hostingSites[bits[0]]
	if f == nil {
		return fmt.Errorf("Don't know how to handle package '%s'", pkg)
	}

	url, rootLen := f(bits)
	if url == "" {
		return fmt.Errorf("Don't know how to handle package '%s'", pkg)
	}

	name := strings.Join(bits[0:rootLen], "/")
	fmt.Fprintf(os.Stderr, "Adding %s\n", url)
	projDir := path.Join("vendor", packageToPath(name))
	if err := v.submoduleAdd(url, projDir); err != nil {
		return err
	}

	return v.process(path.Join("vendor", packageToPath(pkg)), false, true)
}

func (v *vendetta) getGoPath(dir string) (*goPath, error) {
	gp := v.goPaths[dir]
	if gp != nil {
		return gp, nil
	}

	// Get the gopath for the parent directory.  path.Dir doesn't
	// do what we want here in the case where there is no path
	// separator:
	slash := strings.LastIndexByte(dir, os.PathSeparator)
	parentDir := ""
	if slash >= 0 {
		parentDir = dir[:slash]
	}

	gp, err := v.getGoPath(parentDir)
	if err != nil {
		return nil, err
	}

	// If there's a vendor/ dir here, we need to put it on the
	// front of the gopath
	vendorDir := path.Join(dir, "vendor")
	fi, err := os.Stat(v.realDir(vendorDir))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else if fi.IsDir() {
		gp = &goPath{dir: vendorDir, next: gp}
	}

	v.goPaths[dir] = gp
	return gp, nil
}

func (gp *goPath) provides(pkg string, v *vendetta) (bool, string, error) {
	matched, pkg := gp.removePrefix(pkg)
	if !matched {
		return false, "", nil
	}

	foundGoSrc := false
	pkgdir := path.Join(gp.dir, packageToPath(pkg))
	if err := readDir(v.realDir(pkgdir), func(fi os.FileInfo) bool {
		// Should check for symlinks here?
		if fi.Mode().IsRegular() && strings.HasSuffix(fi.Name(), ".go") {
			foundGoSrc = true
			return false
		}
		return true
	}); err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return false, "", err
	}

	return foundGoSrc, pkgdir, nil
}

func (gp *goPath) removePrefix(pkg string) (bool, string) {
	if gp.prefixes == nil {
		return true, pkg
	}

	for prefix := range gp.prefixes {
		if pkg == prefix {
			return true, ""
		} else if strings.HasPrefix(pkg, prefix) &&
			pkg[len(prefix)] == '/' {
			return true, pkg[len(prefix):]
		}
	}

	return false, ""
}

var hostingSites = map[string]func([]string) (string, int){
	"github.com": func(bits []string) (string, int) {
		if len(bits) < 3 {
			return "", 0
		}

		return "https://" + strings.Join(bits[0:3], "/"), 3
	},

	"gopkg.in": func(bits []string) (string, int) {
		if len(bits) < 2 {
			return "", 0
		}

		// Most gopkg.in names are like gopkg.in/pkg.v3
		n := 2
		if !strings.Contains(bits[1], ".") {
			// But some are like gopkg.in/user/pkg.v3
			n = 3

			if len(bits) < 3 {
				return "", 0
			}
		}

		return "https://" + strings.Join(bits[0:n], "/"), n
	},

	"google.golang.org": lookup(2, map[string]string{
		"cloud":     "https://code.googlesource.com/gocloud",
		"grpc":      "https://github.com/grpc/grpc-go",
		"appengine": "https://github.com/golang/appengine",
		"api":       "https://code.googlesource.com/google-api-go-client",
	}),

	"golang.org": lookup(3, map[string]string{
		"net":    "https://go.googlesource.com/net",
		"crypto": "https://go.googlesource.com/crypto",
		"text":   "https://go.googlesource.com/text",
		"oauth2": "https://go.googlesource.com/oauth2",
		"tools":  "https://go.googlesource.com/tools",
		"sys":    "https://go.googlesource.com/sys",
	}),
}

func lookup(n int, m map[string]string) func([]string) (string, int) {
	return func(bits []string) (string, int) {
		if len(bits) < n {
			return "", 0
		}

		url, found := m[bits[n-1]]
		if !found {
			return "", 0
		}

		return url, n
	}
}

// Convert a package name to a filesystem path
func packageToPath(name string) string {
	return strings.Replace(name, "/", string(os.PathSeparator), -1)
}

// Convert a filesystem path to a package name
func pathToPackage(path string) string {
	return strings.Replace(path, string(os.PathSeparator), "/", -1)
}

func readDir(dir string, f func(os.FileInfo) bool) error {
	dh, err := os.Open(dir)
	if err != nil {
		return err
	}

	defer dh.Close()

	for {
		fis, err := dh.Readdir(100)
		if err != nil {
			if err == io.EOF {
				return nil
			}

			return err
		}

		for _, fi := range fis {
			if !f(fi) {
				return nil
			}
		}
	}
}
