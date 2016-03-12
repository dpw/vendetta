// This file contains code taken from the "go" source tree, governed
// by the go license:

// Copyright (c) 2012 The Go Authors. All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package main

import (
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const buildV = false

// From go/src/cmd/go/vcs.go

// securityMode specifies whether a function should make network
// calls using insecure transports (eg, plain text HTTP).
// The zero value is "secure".
type securityMode int

const (
	secure securityMode = iota
	insecure
)

// repoRoot represents a version control system, a repo, and a root of
// where to put it on disk.
type repoRoot struct {
	vcs string

	// repo is the repository URL, including scheme
	repo string

	// root is the import path corresponding to the root of the
	// repository
	root string
}

// repoRootForImportDynamic finds a *repoRoot for a custom domain that's not
// statically known by repoRootForImportPathStatic.
//
// This handles custom import paths like "name.tld/pkg/foo" or just "name.tld".
func queryRepoRoot(importPath string, security securityMode) (*repoRoot, error) {
	slash := strings.Index(importPath, "/")
	if slash < 0 {
		slash = len(importPath)
	}
	host := importPath[:slash]
	if !strings.Contains(host, ".") {
		return nil, errors.New("import path does not begin with hostname")
	}
	urlStr, body, err := httpsOrHTTP(importPath, security)
	if err != nil {
		msg := "https fetch: %v"
		if security == insecure {
			msg = "http/" + msg
		}
		return nil, fmt.Errorf(msg, err)
	}
	defer body.Close()
	imports, err := parseMetaGoImports(body)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %v", importPath, err)
	}
	// Find the matched meta import.
	mmi, err := matchGoImport(imports, importPath)
	if err != nil {
		if err != errNoMatch {
			return nil, fmt.Errorf("parse %s: %v", urlStr, err)
		}
		return nil, fmt.Errorf("parse %s: no go-import meta tags", urlStr)
	}
	if buildV {
		log.Printf("get %q: found meta tag %#v at %s", importPath, mmi, urlStr)
	}
	// If the import was "uni.edu/bob/project", which said the
	// prefix was "uni.edu" and the RepoRoot was "evilroot.com",
	// make sure we don't trust Bob and check out evilroot.com to
	// "uni.edu" yet (possibly overwriting/preempting another
	// non-evil student).  Instead, first verify the root and see
	// if it matches Bob's claim.
	if mmi.Prefix != importPath {
		if buildV {
			log.Printf("get %q: verifying non-authoritative meta tag", importPath)
		}
		urlStr0 := urlStr
		var imports []metaImport
		urlStr, imports, err = metaImportsForPrefix(mmi.Prefix, security)
		if err != nil {
			return nil, err
		}
		metaImport2, err := matchGoImport(imports, importPath)
		if err != nil || mmi != metaImport2 {
			return nil, fmt.Errorf("%s and %s disagree about go-import for %s", urlStr0, urlStr, mmi.Prefix)
		}
	}

	if !strings.Contains(mmi.RepoRoot, "://") {
		return nil, fmt.Errorf("%s: invalid repo root %q; no scheme", urlStr, mmi.RepoRoot)
	}
	return &repoRoot{
		vcs:  mmi.VCS,
		repo: mmi.RepoRoot,
		root: mmi.Prefix,
	}, nil
}

var (
	fetchCacheMu sync.Mutex
	fetchCache   = map[string]fetchResult{} // key is metaImportsForPrefix's importPrefix
)

// metaImportsForPrefix takes a package's root import path as declared in a <meta> tag
// and returns its HTML discovery URL and the parsed metaImport lines
// found on the page.
//
// The importPath is of the form "golang.org/x/tools".
// It is an error if no imports are found.
// urlStr will still be valid if err != nil.
// The returned urlStr will be of the form "https://golang.org/x/tools?go-get=1"
func metaImportsForPrefix(importPrefix string, security securityMode) (urlStr string, imports []metaImport, err error) {
	setCache := func(res fetchResult) fetchResult {
		fetchCacheMu.Lock()
		defer fetchCacheMu.Unlock()
		fetchCache[importPrefix] = res
		return res
	}

	fetch := func() fetchResult {
		fetchCacheMu.Lock()
		if res, ok := fetchCache[importPrefix]; ok {
			fetchCacheMu.Unlock()
			return res
		}
		fetchCacheMu.Unlock()

		urlStr, body, err := httpsOrHTTP(importPrefix, security)
		if err != nil {
			return setCache(fetchResult{urlStr: urlStr, err: fmt.Errorf("fetch %s: %v", urlStr, err)})
		}
		imports, err := parseMetaGoImports(body)
		if err != nil {
			return setCache(fetchResult{urlStr: urlStr, err: fmt.Errorf("parsing %s: %v", urlStr, err)})
		}
		if len(imports) == 0 {
			err = fmt.Errorf("fetch %s: no go-import meta tag", urlStr)
		}
		return setCache(fetchResult{urlStr: urlStr, imports: imports, err: err})
	}

	res := fetch()
	return res.urlStr, res.imports, res.err
}

type fetchResult struct {
	urlStr  string // e.g. "https://foo.com/x/bar?go-get=1"
	imports []metaImport
	err     error
}

// metaImport represents the parsed <meta name="go-import"
// content="prefix vcs reporoot" /> tags from HTML files.
type metaImport struct {
	Prefix, VCS, RepoRoot string
}

// errNoMatch is returned from matchGoImport when there's no applicable match.
var errNoMatch = errors.New("no import match")

// matchGoImport returns the metaImport from imports matching importPath.
// An error is returned if there are multiple matches.
// errNoMatch is returned if none match.
func matchGoImport(imports []metaImport, importPath string) (_ metaImport, err error) {
	match := -1
	for i, im := range imports {
		if !strings.HasPrefix(importPath, im.Prefix) {
			continue
		}
		if match != -1 {
			err = fmt.Errorf("multiple meta tags match import path %q", importPath)
			return
		}
		match = i
	}
	if match == -1 {
		err = errNoMatch
		return
	}
	return imports[match], nil
}

// From go/src/cmd/go/http.go

// httpClient is the default HTTP client, but a variable so it can be
// changed by tests, without modifying http.DefaultClient.
var httpClient = http.DefaultClient

// impatientInsecureHTTPClient is used in -insecure mode,
// when we're connecting to https servers that might not be there
// or might be using self-signed certificates.
var impatientInsecureHTTPClient = &http.Client{
	Timeout: time.Duration(5 * time.Second),
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	},
}

type httpError struct {
	status     string
	statusCode int
	url        string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s: %s", e.url, e.status)
}

// httpGET returns the data from an HTTP GET request for the given URL.
func httpGET(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		err := &httpError{status: resp.Status, statusCode: resp.StatusCode, url: url}

		return nil, err
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", url, err)
	}
	return b, nil
}

// httpsOrHTTP returns the body of either the importPath's
// https resource or, if unavailable, the http resource.
func httpsOrHTTP(importPath string, security securityMode) (urlStr string, body io.ReadCloser, err error) {
	fetch := func(scheme string) (urlStr string, res *http.Response, err error) {
		u, err := url.Parse(scheme + "://" + importPath)
		if err != nil {
			return "", nil, err
		}
		u.RawQuery = "go-get=1"
		urlStr = u.String()
		if buildV {
			log.Printf("Fetching %s", urlStr)
		}
		if security == insecure && scheme == "https" { // fail earlier
			res, err = impatientInsecureHTTPClient.Get(urlStr)
		} else {
			res, err = httpClient.Get(urlStr)
		}
		return
	}
	closeBody := func(res *http.Response) {
		if res != nil {
			res.Body.Close()
		}
	}
	urlStr, res, err := fetch("https")
	if err != nil {
		if buildV {
			log.Printf("https fetch failed: %v", err)
		}
		if security == insecure {
			closeBody(res)
			urlStr, res, err = fetch("http")
		}
	}
	if err != nil {
		closeBody(res)
		return "", nil, err
	}
	// Note: accepting a non-200 OK here, so people can serve a
	// meta import in their http 404 page.
	if buildV {
		log.Printf("Parsing meta tags from %s (status code %d)", urlStr, res.StatusCode)
	}
	return urlStr, res.Body, nil
}

// From go/src/cmd/go/discovery.go

// charsetReader returns a reader for the given charset. Currently
// it only supports UTF-8 and ASCII. Otherwise, it returns a meaningful
// error which is printed by go get, so the user can find why the package
// wasn't downloaded if the encoding is not supported. Note that, in
// order to reduce potential errors, ASCII is treated as UTF-8 (i.e. characters
// greater than 0x7f are not rejected).
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "ascii":
		return input, nil
	default:
		return nil, fmt.Errorf("can't decode XML document using charset %q", charset)
	}
}

// parseMetaGoImports returns meta imports from the HTML in r.
// Parsing ends at the end of the <head> section or the beginning of the <body>.
func parseMetaGoImports(r io.Reader) (imports []metaImport, err error) {
	d := xml.NewDecoder(r)
	d.CharsetReader = charsetReader
	d.Strict = false
	var t xml.Token
	for {
		t, err = d.RawToken()
		if err != nil {
			if err == io.EOF || len(imports) > 0 {
				err = nil
			}
			return
		}
		if e, ok := t.(xml.StartElement); ok && strings.EqualFold(e.Name.Local, "body") {
			return
		}
		if e, ok := t.(xml.EndElement); ok && strings.EqualFold(e.Name.Local, "head") {
			return
		}
		e, ok := t.(xml.StartElement)
		if !ok || !strings.EqualFold(e.Name.Local, "meta") {
			continue
		}
		if attrValue(e.Attr, "name") != "go-import" {
			continue
		}
		if f := strings.Fields(attrValue(e.Attr, "content")); len(f) == 3 {
			imports = append(imports, metaImport{
				Prefix:   f[0],
				VCS:      f[1],
				RepoRoot: f[2],
			})
		}
	}
}

// attrValue returns the attribute value for the case-insensitive key
// `name', or the empty string if nothing is found.
func attrValue(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}
