package main

import (
	"bufio"
	"fmt"
	"go/build"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
)

// TODO:
// Check that start dir is git repo, determine names from git config, or GOPATH
// prune support
// update support
// proper go-import meta tag handling
// exhaustive option

func main() {
	state := state{
		root:          os.Args[1],
		processedDirs: make(map[string]struct{}),
	}

	state.addProject(project{name: os.Args[2], dir: ""})

	if err := state.populate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := state.processRecursive("", true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type state struct {
	root          string
	projects      []project
	processedDirs map[string]struct{}
}

type project struct {
	name string
	dir  string
}

func (state *state) populate() error {
	cmd := exec.Command("git", "-C", state.root, "submodule", "status")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	vendorPref := "vendor" + string(os.PathSeparator)

	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		fields := strings.Split(strings.TrimSpace(lines.Text()), " ")
		path := fields[1]

		if strings.HasPrefix(path, vendorPref) {
			state.addProject(project{
				name: pathToPackage(path[len(vendorPref):]),
				dir:  path,
			})
		}
	}

	if err := lines.Err(); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		return err
	}

	return nil
}

func (state *state) processRecursive(dir string, root bool) error {
	if err := state.process(dir, true); err != nil {
		return err
	}

	var subdirs []string
	if err := readDir(path.Join(state.root, dir), func(fi os.FileInfo) bool {
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

		err := state.processRecursive(path.Join(dir, subdir), false)
		if err != nil {
			return err
		}
	}

	return nil
}

func (state *state) process(dir string, testsToo bool) error {
	if _, found := state.processedDirs[dir]; found {
		return nil
	}

	state.processedDirs[dir] = struct{}{}

	pkg, err := build.Default.ImportDir(path.Join(state.root, dir), 0)
	if err != nil {
		if _, ok := err.(*build.NoGoError); ok {
			return nil
		}

		return err
	}

	deps := func(imports []string) error {
		for _, imp := range imports {
			if err := state.dependency(imp); err != nil {
				return err
			}
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

func hasPrefixPath(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) &&
		len(s) > len(prefix) && s[len(prefix)] == '/'
}

func (state *state) dependency(pkg string) error {
	dir, err := state.resolvePackage(pkg)
	if err != nil || dir == "" {
		return err
	}

	return state.process(dir, false)
}

func (state *state) resolvePackage(pkg string) (string, error) {
	proj, found := state.findPackageProject(pkg)
	if !found {
		// Figure out what a package name means. This is a rough
		// approximation of what golang's vcs.go does:
		bits := strings.Split(pkg, "/")

		// Exclude golang standard packages
		if !strings.Contains(bits[0], ".") {
			return "", nil
		}

		f := hostingSites[bits[0]]
		if f == nil {
			return "", fmt.Errorf("Don't know how to handle package '%s'", pkg)
		}

		url, rootLen := f(bits)
		if url == "" {
			return "", fmt.Errorf("Don't know how to handle package '%s'", pkg)
		}

		proj.name = strings.Join(bits[0:rootLen], "/")
		proj.dir = path.Join("vendor", packageToPath(proj.name))
		if err := state.submoduleAdd(url, proj.dir); err != nil {
			return "", err
		}

		state.addProject(proj)
	}

	if pkg == proj.name {
		return proj.dir, nil
	}

	return path.Join(proj.dir, packageToPath(pkg[len(proj.name)+1:])), nil
}

func (state *state) findPackageProject(pkg string) (project, bool) {
	i := sort.Search(len(state.projects), func(i int) bool {
		return state.projects[i].name >= pkg
	})

	if i < len(state.projects) && state.projects[i].name == pkg {
		return state.projects[i], true
	}

	if i > 0 && hasPrefixPath(pkg, state.projects[i-1].name) {
		return state.projects[i-1], true
	}

	return project{}, false
}

func (state *state) addProject(proj project) {
	i := sort.Search(len(state.projects), func(i int) bool {
		return state.projects[i].name >= proj.name
	})

	projects := make([]project, len(state.projects)+1)
	copy(projects, state.projects[:i])
	projects[i] = proj
	copy(projects[i+1:], state.projects[i:])
	state.projects = projects
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

func (state *state) submoduleAdd(url, dir string) error {
	fmt.Fprintln(os.Stderr, "Adding", url)
	out, err := exec.Command("git", "-C", state.root, "submodule", "add",
		url, dir).CombinedOutput()
	if err != nil {
		os.Stderr.Write(out)
	}

	return err
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
