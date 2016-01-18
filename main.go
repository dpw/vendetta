package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
)

// TODO:
//
// eliminate empty dirs after pruning
//
// proper go-import meta tag handling
//
// popen should include command in errors
//
// warn when it looks like a package ought to be present at the
// particular path, but it's not.  E.g. when resolving an import of
// github.com/foo/bar/baz, we find github.com/foo.
//
// check that declared package names match dirs
//
// infer project name from GOPATH
//
// use type aliases for packages and paths?

type config struct {
	rootDir     string
	projectName string
	update      bool
	prune       bool
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [ <project directory> ]\n",
			os.Args[0])
		flag.PrintDefaults()
	}

	var cf config

	flag.StringVar(&cf.projectName, "n", "",
		"base package name for the project, e.g. github.com/user/proj")
	flag.BoolVar(&cf.update, "u", false,
		"update dependency submodules from their remote repos")
	flag.BoolVar(&cf.prune, "p", false,
		"prune unused dependency submodules")

	flag.Parse()

	cf.rootDir = "."
	switch {
	case flag.NArg() == 1:
		cf.rootDir = flag.Arg(1)
	case flag.NArg() > 1:
		flag.Usage()
		os.Exit(2)
	}

	if err := run(&cf); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type vendetta struct {
	*config
	goPath
	goPaths       map[string]*goPath
	processedDirs map[string]struct{}
	submodules    []submodule
	trace         []trace
}

type trace struct {
	dir string
	pkg string
}

type submodule struct {
	dir     string
	updated bool
	used    bool
}

// Conceptually, a gopath element.  These are arranged into linked
// lists, representing the gopath applicable for a particular
// directory (produced by getGoPath and memoized in the goPaths map).
type goPath struct {
	dir      string
	prefixes map[string]struct{}
	next     *goPath
}

func run(cf *config) error {
	v := vendetta{
		config:        cf,
		goPaths:       make(map[string]*goPath),
		processedDirs: make(map[string]struct{}),
	}

	v.goPaths[""] = &goPath{dir: "vendor", next: &v.goPath}
	v.prefixes = make(map[string]struct{})

	if cf.projectName != "" {
		v.prefixes[cf.projectName] = struct{}{}
	} else {
		if err := v.inferProjectNameFromGit(); err != nil {
			return err
		}

		if len(v.prefixes) == 0 {
			return fmt.Errorf("Unable to infer project name; specify it explicitly with the '-p' option.")
		}
	}

	if err := v.checkSubmodules(); err != nil {
		return err
	}

	if err := v.populateSubmodules(); err != nil {
		return err
	}

	if err := v.processRecursive("", true); err != nil {
		return err
	}

	return v.pruneSubmodules()
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
		if len(fields) < 2 {
			return fmt.Errorf("could not parse 'git remote' output")
		}

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

// Check for submodules that seem to be missing in the working tree.
func (v *vendetta) checkSubmodules() error {
	var err2 error
	if err := v.querySubmodules(func(path string) bool {
		err2 = v.checkSubmodule(path)
		return err2 == nil
	}, "--recursive"); err != nil {
		return err
	}

	return err2
}

func (v *vendetta) checkSubmodule(dir string) error {
	foundSomething := false
	if err := readDir(dir, func(fi os.FileInfo) bool {
		foundSomething = true
		return false
	}); err != nil && !os.IsNotExist(err) {
		return err
	}

	if !foundSomething {
		return fmt.Errorf("The submodule '%s' doesn't seem to the present in the working tree.  Maybe you forgot to update with 'git submodule update --init --recursive'?", dir)
	}

	return nil
}

func (v *vendetta) querySubmodules(f func(string) bool, args ...string) error {
	args = append([]string{"-C", v.rootDir, "submodule", "status"}, args...)
	status, err := popen("git", args...)
	if err != nil {
		return err
	}

	defer status.close()

	for status.Scan() {
		fields := splitWS(strings.TrimSpace(status.Text()))
		if len(fields) < 2 {
			return fmt.Errorf("could not parse 'git submodule status' output")
		}

		path := fields[1]

		if !f(path) {
			return nil
		}
	}

	return status.close()
}

func (v *vendetta) populateSubmodules() error {
	var submodules []string
	if err := v.querySubmodules(func(path string) bool {
		submodules = append(submodules, path)
		return true
	}); err != nil {
		return err
	}

	sort.Strings(submodules)

	v.submodules = make([]submodule, 0, len(submodules))
	for _, p := range submodules {
		v.submodules = append(v.submodules, submodule{dir: p})
	}

	return nil
}

func (v *vendetta) pathInSubmodule(path string) *submodule {
	i := sort.Search(len(v.submodules), func(i int) bool {
		return v.submodules[i].dir >= path
	})
	if i < len(v.submodules) && v.submodules[i].dir == path {
		return &v.submodules[i]
	}
	if i > 0 && isSubpath(path, v.submodules[i-1].dir) {
		return &v.submodules[i-1]
	}
	return nil
}

func (v *vendetta) addSubmodule(dir string) {
	i := sort.Search(len(v.submodules), func(i int) bool {
		return v.submodules[i].dir >= dir
	})

	submodules := make([]submodule, len(v.submodules)+1)
	copy(submodules, v.submodules[:i])
	submodules[i] = submodule{dir: dir, updated: true, used: true}
	copy(submodules[i+1:], v.submodules[i:])
	v.submodules = submodules
}

func isSubpath(path, dir string) bool {
	return path == dir ||
		(strings.HasPrefix(path, dir) && path[len(dir)] == os.PathSeparator)
}

func (v *vendetta) updateSubmodule(sm *submodule) error {
	if sm.updated {
		return nil
	}

	sm.updated = true
	fmt.Fprintf(os.Stderr, "Updating submodule %s from remote\n", sm.dir)
	return v.git("submodule", "update", "--remote", "--recursive", sm.dir)
}

func (v *vendetta) pruneSubmodules() error {
	for _, sm := range v.submodules {
		if sm.used || !isSubpath(sm.dir, "vendor") {
			continue
		}

		if v.prune {
			fmt.Fprintf(os.Stderr, "Removing unused submodule %s\n",
				sm.dir)
			if err := v.git("rm", "-f", sm.dir); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "Unused submodule %s (use -p option to prune)\n", sm.dir)
		}
	}

	return nil
}

var wsRE = regexp.MustCompile(`[ \t]+`)

func splitWS(s string) []string {
	return wsRE.Split(s, -1)
}

func (v *vendetta) gitSubmoduleAdd(url, dir string) error {
	fmt.Fprintf(os.Stderr, "Adding %s at %s\n", url, dir)
	err := v.git("submodule", "add", url, dir)
	if err != nil {
		return err
	}

	v.addSubmodule(dir)
	return nil
}

func (v *vendetta) git(args ...string) error {
	return system("git", append([]string{"-C", v.rootDir}, args...)...)
}

func system(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
		if err == nil {
			return nil
		}
	}

	return fmt.Errorf("Command failed: %s %s (%s)",
		name, strings.Join(args, " "), err)
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
	found, pkgdir, err := v.searchGoPath(dir, pkg)
	switch {
	case err != nil:
		return err
	case found:
		// Does it fall within an existing submodule
		// args..under vendor/ ?
		if sm := v.pathInSubmodule(pkgdir); sm != nil {
			sm.used = true
			if v.update {
				if err := v.updateSubmodule(sm); err != nil {
					return err
				}
			}
		}

		return v.process(pkgdir, false, true)
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
	projDir := path.Join("vendor", packageToPath(name))
	if err := v.gitSubmoduleAdd(url, projDir); err != nil {
		return err
	}

	return v.process(path.Join("vendor", packageToPath(pkg)), false, true)
}

// Search the gopath for the given dir to find an existing package
func (v *vendetta) searchGoPath(dir, pkg string) (bool, string, error) {
	gp, err := v.getGoPath(dir)
	if err != nil {
		return false, "", err
	}

	for gp != nil {
		found, pkgdir, err := gp.provides(pkg, v)
		if err != nil {
			return false, "", err
		}

		if found {
			return found, pkgdir, nil
		}

		gp = gp.next
	}

	return false, "", nil
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
		} else if isSubpath(pkg, prefix) {
			return true, pkg[len(prefix)+1:]
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
