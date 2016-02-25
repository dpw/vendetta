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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// TODO:
//
// Don't need a project name if everything is in package main
//
// check the directory is a git repo, and error if not
//
// option to run in any directory of git repo.  Needs to figure out
// path from repo root.
//
// verbose option to print git commands being run
//
// popen should include command in errors
//
// Deal with git being fussy when a submodule is removed then re-added
//
// warn when it looks like a package ought to be present at the
// particular path, but it's not.  E.g. when resolving an import of
// github.com/foo/bar/baz, we find github.com/foo.
//
// check that declared package names match dirs
//
// Support relative imports
//
// Infer project name from import comments

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

	switch {
	case flag.NArg() == 1:
		cf.rootDir = flag.Arg(0)
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
	goPaths     map[string]*goPath
	dirPackages map[string]*build.Package
	submodules  []submodule
}

// A goPath says where to search for packages (analogous to
// GOPATH). Different directories have different gopaths because there
// can be vendor directories anywhere, not just at the top level.
// These gopaths are produced by getGoPath and memoized in the goPaths
// map.
type goPath struct {
	dir  string
	next *goPath

	// prefixes is nil for goPaths corresponding to vendor
	// directories.  But there is also a goPath corresponding to
	// the top-level project directory.  When searching for a
	// package in that top-level directory, we need to remove any
	// prefix of the package name corresponding to the root name
	// of the project (e.g. github.com/user/proj).  prefixes is
	// the set of such prefixes.
	prefixes map[string]struct{}
}

type submodule struct {
	dir  string
	used bool
}

func run(cf *config) error {
	v := vendetta{
		config:      cf,
		goPaths:     make(map[string]*goPath),
		dirPackages: make(map[string]*build.Package),
	}

	v.goPaths[""] = &goPath{dir: "vendor", next: &v.goPath}
	v.prefixes = make(map[string]struct{})

	if cf.projectName != "" {
		v.prefixes[cf.projectName] = struct{}{}
	} else {
		if err := v.inferProjectNameFromGoPath(); err != nil {
			return err
		}

		if err := v.inferProjectNameFromGit(); err != nil {
			return err
		}

		if len(v.prefixes) == 0 {
			return fmt.Errorf("Unable to infer project name; specify it explicitly with the '-n' option.")
		}
	}

	if err := v.checkSubmodules(); err != nil {
		return err
	}

	if err := v.populateSubmodules(); err != nil {
		return err
	}

	if err := v.scanRootProject(); err != nil {
		return err
	}

	return v.pruneSubmodules()
}

// Attempt to infer the project name from GOPATH, by seeing if the
// project dir resides under any element of the GOPATH.
func (v *vendetta) inferProjectNameFromGoPath() error {
	gp := os.Getenv("GOPATH")
	if gp == "" {
		return nil
	}

	var gpfis []os.FileInfo
	for _, p := range filepath.SplitList(gp) {
		fi, err := os.Stat(filepath.Join(p, "src"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return err
		}

		gpfis = append(gpfis, fi)
	}

	dir := v.rootDir
	if dir == "" {
		dir = "."
	}

	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	var proj, projsep string
	for {
		var subdir string
		dir, subdir = filepath.Split(dir)

		proj = subdir + projsep + proj
		projsep = "/"

		fi, err := os.Stat(dir)
		if err != nil {
			return err
		}

		for _, gpfi := range gpfis {
			if os.SameFile(fi, gpfi) {
				v.inferredProjectName(proj, "GOPATH")
				return nil
			}
		}

		if dir != "" && dir[len(dir)-1] == os.PathSeparator {
			dir = dir[:len(dir)-1]
		}

		if dir == "" {
			return nil
		}
	}
}

var remoteUrlRE = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:)(.*\.?)$`)

func (v *vendetta) inferProjectNameFromGit() error {
	remotes, err := v.popen("git", "remote", "-v")
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

			v.inferredProjectName("github.com/"+name,
				"git remote")
		}
	}

	if err := remotes.close(); err != nil {
		return err
	}

	return nil
}

func (v *vendetta) inferredProjectName(proj, source string) {
	if _, found := v.prefixes[proj]; !found {
		fmt.Println("Inferred root package name", proj, "from", source)
		v.prefixes[proj] = struct{}{}
	}
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
	if err := readDir(v.realDir(dir), func(fi os.FileInfo) bool {
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
	status, err := v.popen("git",
		append([]string{"submodule", "status"}, args...)...)
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
	submodules[i] = submodule{dir: dir, used: true}
	copy(submodules[i+1:], v.submodules[i:])
	v.submodules = submodules
}

func isSubpath(path, dir string) bool {
	return path == dir ||
		(strings.HasPrefix(path, dir) && path[len(dir)] == os.PathSeparator)
}

func (v *vendetta) updateSubmodule(sm *submodule) error {
	fmt.Fprintf(os.Stderr, "Updating submodule %s from remote\n", sm.dir)
	if err := v.git("submodule", "update", "--remote", "--recursive", sm.dir); err != nil {
		return err
	}

	// If we don't put the updated submodule into the index, a
	// subsequent "git submodule update" will revert it, which can
	// lead to surprises.
	return v.git("add", sm.dir)
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

			if err := v.removeEmptyDirsAbove(sm.dir); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "Unused submodule %s (use -p option to prune)\n", sm.dir)
		}
	}

	return nil
}

func (v *vendetta) removeEmptyDirsAbove(dir string) error {
	for {
		dir = parentDir(dir)
		if dir == "" {
			return nil
		}

		empty := true
		if err := readDir(v.realDir(dir), func(_ os.FileInfo) bool {
			empty = false
			return false
		}); err != nil {
			return err
		}

		if !empty {
			return nil
		}

		if err := os.Remove(v.realDir(dir)); err != nil {
			return err
		}
	}
}

// Get the directory name from a path.  path.Dir doesn't
// do what we want in the case where there is no path
// separator:
func parentDir(path string) string {
	slash := strings.LastIndexByte(path, os.PathSeparator)
	dir := ""
	if slash >= 0 {
		dir = path[:slash]
	}
	return dir
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
	return v.system("git", args...)
}

func (v *vendetta) system(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = v.rootDir
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

func (v *vendetta) popen(name string, args ...string) (popenLines, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = v.rootDir
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
	res := filepath.Join(v.rootDir, dir)
	if res == "" {
		res = "."
	}

	return res
}

func (v *vendetta) scanRootProject() error {
	// Collect all root project package directories
	var dirs []string
	var err error

	var traverseDir func(dir string, root bool)
	traverseDir = func(dir string, root bool) {
		dirs = append(dirs, dir)
		err = readDir(v.realDir(dir), func(fi os.FileInfo) bool {
			if !fi.IsDir() {
				return true
			}

			switch fi.Name() {
			case "vendor":
				if root {
					return true
				}
			case "testdata":
				return true
			}

			traverseDir(filepath.Join(dir, fi.Name()), false)
			return err == nil
		})
	}

	traverseDir("", true)
	if err != nil {
		return err
	}

	// Load each package, without resolving dependencies, because
	// we process packages in the root project slightly
	// differently to dependency packages.
	for _, dir := range dirs {
		_, err := v.loadPackage(dir, true)
		if err != nil {
			return err
		}
	}

	// Now resolve dependencies
	for _, dir := range dirs {
		pkg := v.dirPackages[dir]
		if pkg == nil {
			continue
		}

		if err = v.resolveDependencies(dir, pkg.Imports); err != nil {
			return err
		}
		if err = v.resolveDependencies(dir, pkg.TestImports); err != nil {
			return err
		}
	}

	return nil
}

func (v *vendetta) scanPackage(dir string) (*build.Package, error) {
	if pkg := v.dirPackages[dir]; pkg != nil {
		return pkg, nil
	}

	pkg, err := v.loadPackage(dir, false)
	if err != nil {
		return nil, err
	}

	if err = v.resolveDependencies(dir, pkg.Imports); err != nil {
		return nil, err
	}

	return pkg, nil
}

func (v *vendetta) loadPackage(dir string, noGoOk bool) (*build.Package, error) {
	pkg, err := build.Default.ImportDir(v.realDir(dir), build.ImportComment)
	if err != nil {
		if _, ok := err.(*build.NoGoError); ok && noGoOk {
			return nil, nil
		}

		return nil, fmt.Errorf("gathering imports in %s: %s",
			v.realDir(dir), err)
	}

	v.dirPackages[dir] = pkg
	return pkg, nil
}

func (v *vendetta) resolveDependencies(dir string, deps []string) error {
	for _, dep := range deps {
		if err := v.resolveDependency(dir, dep); err != nil {
			return err
		}
	}

	return nil
}

func (v *vendetta) resolveDependency(dir string, pkg string) error {
	found, pkgdir, err := v.searchGoPath(dir, pkg)
	switch {
	case err != nil:
		return err
	case found:
		// Does the package fall within an existing submodule
		// under vendor/ ?
		if sm := v.pathInSubmodule(pkgdir); sm != nil && !sm.used {
			sm.used = true
			if v.update {
				if err := v.updateSubmodule(sm); err != nil {
					return err
				}
			}
		}

	default:
		pkgdir, err = v.obtainPackage(pkg)
		if err != nil || pkgdir == "" {
			return err
		}
	}

	pi, err := v.scanPackage(pkgdir)
	if err != nil {
		return err
	}

	if pi.ImportComment != "" && pkg != pi.ImportComment {
		fmt.Printf("Warning: Package with import comment %s referred to as %s (from directory %s)\n",
			pi.ImportComment, pkg, v.realDir(dir))
	}

	return nil
}

func (v *vendetta) obtainPackage(pkg string) (string, error) {
	bits := strings.Split(pkg, "/")

	// Exclude golang standard packages
	if !strings.Contains(bits[0], ".") {
		return "", nil
	}

	// Figure out how to obtain the package.  Packages on
	// github.com are treated as a special case, because that is
	// most of them.  Otherwise, we use the queryRepoRoot code
	// borrowed from vcs.go to figure out how to obtain the
	// package.
	var rootPkg, url string
	if bits[0] == "github.com" {
		if len(bits) < 3 {
			return "", fmt.Errorf("github.com package name %s seems to be truncated", pkg)
		}

		rootPkg = strings.Join(bits[:3], "/")
		url = "https://" + rootPkg
	} else {
		rr, err := queryRepoRoot(pkg, secure)
		if err != nil {
			return "", err
		}

		if rr.vcs != "git" {
			return "", fmt.Errorf("Package %s does not live in a git repo", pkg)
		}

		rootPkg = rr.root
		url = rr.repo
	}

	projDir := filepath.Join("vendor", packageToPath(rootPkg))
	if err := v.gitSubmoduleAdd(url, projDir); err != nil {
		return "", err
	}

	return filepath.Join("vendor", packageToPath(pkg)), nil
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

	gp, err := v.getGoPath(parentDir(dir))
	if err != nil {
		return nil, err
	}

	// If there's a vendor/ dir here, we need to put it on the
	// front of the gopath
	vendorDir := filepath.Join(dir, "vendor")
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
	pkgdir := filepath.Join(gp.dir, packageToPath(pkg))
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

// Convert a package name to a filesystem path
func packageToPath(name string) string {
	return filepath.FromSlash(name)
}

// Convert a filesystem path to a package name
func pathToPackage(path string) string {
	return filepath.ToSlash(path)
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
