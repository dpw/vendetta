# Vendetta

The go dependency management tool for people who don't like go
dependency management tools.

## Introduction

Vendetta is a minimal tool for managing the dependencies in the
`vendor` directory of a go project.  Go supports such directories
[from version 1.5](https://golang.org/s/go15vendor), but doesn't
provide a way to manage their contents.  Vendetta is less obtrusive
than other go dependency management tools because it relies on git
submodules.  You don't need vendetta to build a project, or for most
other development tasks.  Vendetta is just used to populate the
`vendor` directory with submodules, or to update them when a project's
dependencies change.  Because it uses git submodules, your project
repository remains small, and it is easy to relate the contents of the
`vendor` directory back to their origin repositories.

## Installation

If you have your GOPATH set up:

```sh
go get github.com/dpw/vendetta
```

This will install `vendetta` in `$GOPATH/bin`

If you don't:

```sh
git clone https://github.com/dpw/vendetta.git && (cd vendetta ; go build)
```

The `vendetta` binary will be in the cloned `vendetta` directory.

## Use

Usage: `vendetta `_`[options] [directory]`_

The directory specified should be the top-level directory of the git
repo that holds your Go project.  If it is omitted, the current
directory is used.

Like `go get`, vendetta identifies any missing packages needed to
build your top-level project (including packages needed by other
dependencies).  It then finds the projects containing those missing
packages, and runs the git commands to add submodules for them.

When you clone a project with submodules, as produced by vendetta, the
submodule directories will initially be empty.  Do `git submodule
update --init --recursive` in order to retrieve the submodule
contents.

Vendetta follows all the relevant Go conventions, such as ignoring
`testdata` directories.

### Options

* `-p`: _Prune_ unneeded submodules under `vendor/`.

* `-u`: _Update_ dependencies of your project.  This pulls from the
  remote repositories for required submodules under `vendor/`.

## Background

Go 1.5 introduced the [Go Vendor](https://golang.org/s/go15vendor)
feature.  This provides support in the standard go tool set for
`vendor` directories which contain the source code for dependencies of
a project.  Note that in Go 1.5, you must set the
GO15VENDOREXPERIMENT environment variable to enable this feature; but
Go 1.6 enables it by default.

The Go Vendor feature is a significant step forward for dependency
management in go.  But out of the box, it does not provide a way to
populate the `vendor` directory for a project with its dependencies,
or manage those dependencies as the project evolves.  Trying to do
this by hand is cumbersome and error prone.

[Other go vendoring tools are
available](https://github.com/golang/go/wiki/PackageManagementTools).
But they support two approaches: Either they copy the source code of
dependencies into `vendor/`, which bloats the repository of your
project.  Or, they write a dependency metadata file under `vendor/`
which says how to get the dependencies.  But then anyone who wants to
build the project needs to use a specific tool to retrieve the
dependencies. (And there is no dominant standard for the dependency
metadata files – there are even two different formats for a file
called `vendor.json`.)

Instead, vendetta relies on the
[submodule](https://git-scm.com/docs/git-submodule) feature of git,
which provides a way for one git repository to point to another git
repository (and a specific commit within it).  And submodules are a
standard feature of git, so git will retrieve them for you.  You may
already have experience with submodules.  And tools built on top of
git understand submodules (e.g. github knows about submodules, and
will display a submodule pointing to another project on github as a
link).

When you clone a repository containing submodules, you need to do `git
submodule update --init --recursive` in order to retrieve the
submodule contents. This step is sometimes surprising to those new to
git submodules.  But it can be hidden by incorporating it into build
scripts or makefiles.  And `go get` will do `git submodule update`
after cloning a repo, so it is not necessary to run it explicitly when
fetching go packages in that way.

A downside of git submodules is that, being a git-specific feature,
they only support dependencies in git.  But given the dominance of git
with the go community, this is not much of a limitation.
