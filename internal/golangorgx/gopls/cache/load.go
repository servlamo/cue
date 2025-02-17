// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"cuelang.org/go/internal/golangorgx/gopls/cache/metadata"
	"cuelang.org/go/internal/golangorgx/gopls/protocol"
	"cuelang.org/go/internal/golangorgx/gopls/util/bug"
	"cuelang.org/go/internal/golangorgx/gopls/util/immutable"
	"cuelang.org/go/internal/golangorgx/gopls/util/pathutil"
	"cuelang.org/go/internal/golangorgx/gopls/util/slices"
	"cuelang.org/go/internal/golangorgx/tools/packagesinternal"
	"golang.org/x/tools/go/packages"
)

var loadID uint64 // atomic identifier for loads

// errNoPackages indicates that a load query matched no packages.
var errNoPackages = errors.New("no packages returned")

// load calls packages.Load for the given scopes, updating package metadata,
// import graph, and mapped files with the result.
//
// The resulting error may wrap the moduleErrorMap error type, representing
// errors associated with specific modules.
//
// If scopes contains a file scope there must be exactly one scope.
func (s *Snapshot) load(ctx context.Context, allowNetwork bool, scopes ...loadScope) (err error) {
	_ = atomic.AddUint64(&loadID, 1)

	return nil
}

type moduleErrorMap struct {
	errs map[string][]packages.Error // module path -> errors
}

func (m *moduleErrorMap) Error() string {
	var paths []string // sort for stability
	for path, errs := range m.errs {
		if len(errs) > 0 { // should always be true, but be cautious
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%d modules have errors:\n", len(paths))
	for _, path := range paths {
		fmt.Fprintf(&buf, "\t%s:%s\n", path, m.errs[path][0].Msg)
	}

	return buf.String()
}

// buildMetadata populates the updates map with metadata updates to
// apply, based on the given pkg. It recurs through pkg.Imports to ensure that
// metadata exists for all dependencies.
func buildMetadata(updates map[PackageID]*metadata.Package, pkg *packages.Package, loadDir string, standalone bool) {
	// Allow for multiple ad-hoc packages in the workspace (see #47584).
	pkgPath := PackagePath(pkg.PkgPath)
	id := PackageID(pkg.ID)

	if metadata.IsCommandLineArguments(id) {
		var f string // file to use as disambiguating suffix
		if len(pkg.CompiledGoFiles) > 0 {
			f = pkg.CompiledGoFiles[0]

			// If there are multiple files,
			// we can't use only the first.
			// (Can this happen? #64557)
			if len(pkg.CompiledGoFiles) > 1 {
				bug.Reportf("unexpected files in command-line-arguments package: %v", pkg.CompiledGoFiles)
				return
			}
		} else if len(pkg.IgnoredFiles) > 0 {
			// A file=empty.go query results in IgnoredFiles=[empty.go].
			f = pkg.IgnoredFiles[0]
		} else {
			bug.Reportf("command-line-arguments package has neither CompiledGoFiles nor IgnoredFiles: %#v", "") //*pkg.Metadata)
			return
		}
		id = PackageID(pkg.ID + f)
		pkgPath = PackagePath(pkg.PkgPath + f)
	}

	// Duplicate?
	if _, ok := updates[id]; ok {
		// A package was encountered twice due to shared
		// subgraphs (common) or cycles (rare). Although "go
		// list" usually breaks cycles, we don't rely on it.
		// breakImportCycles in metadataGraph.Clone takes care
		// of it later.
		return
	}

	if pkg.TypesSizes == nil {
		panic(id + ".TypeSizes is nil")
	}

	// Recreate the metadata rather than reusing it to avoid locking.
	mp := &metadata.Package{
		ID:         id,
		PkgPath:    pkgPath,
		Name:       PackageName(pkg.Name),
		ForTest:    PackagePath(packagesinternal.GetForTest(pkg)),
		TypesSizes: pkg.TypesSizes,
		LoadDir:    loadDir,
		Module:     pkg.Module,
		Errors:     pkg.Errors,
		DepsErrors: packagesinternal.GetDepsErrors(pkg),
		Standalone: standalone,
	}

	updates[id] = mp

	for _, filename := range pkg.CompiledGoFiles {
		uri := protocol.URIFromPath(filename)
		mp.CompiledGoFiles = append(mp.CompiledGoFiles, uri)
	}
	for _, filename := range pkg.GoFiles {
		uri := protocol.URIFromPath(filename)
		mp.GoFiles = append(mp.GoFiles, uri)
	}
	for _, filename := range pkg.IgnoredFiles {
		uri := protocol.URIFromPath(filename)
		mp.IgnoredFiles = append(mp.IgnoredFiles, uri)
	}

	depsByImpPath := make(map[ImportPath]PackageID)
	depsByPkgPath := make(map[PackagePath]PackageID)
	for importPath, imported := range pkg.Imports {
		importPath := ImportPath(importPath)

		// It is not an invariant that importPath == imported.PkgPath.
		// For example, package "net" imports "golang.org/x/net/dns/dnsmessage"
		// which refers to the package whose ID and PkgPath are both
		// "vendor/golang.org/x/net/dns/dnsmessage". Notice the ImportMap,
		// which maps ImportPaths to PackagePaths:
		//
		// $ go list -json net vendor/golang.org/x/net/dns/dnsmessage
		// {
		// 	"ImportPath": "net",
		// 	"Name": "net",
		// 	"Imports": [
		// 		"C",
		// 		"vendor/golang.org/x/net/dns/dnsmessage",
		// 		"vendor/golang.org/x/net/route",
		// 		...
		// 	],
		// 	"ImportMap": {
		// 		"golang.org/x/net/dns/dnsmessage": "vendor/golang.org/x/net/dns/dnsmessage",
		// 		"golang.org/x/net/route": "vendor/golang.org/x/net/route"
		// 	},
		//      ...
		// }
		// {
		// 	"ImportPath": "vendor/golang.org/x/net/dns/dnsmessage",
		// 	"Name": "dnsmessage",
		//      ...
		// }
		//
		// (Beware that, for historical reasons, go list uses
		// the JSON field "ImportPath" for the package's
		// path--effectively the linker symbol prefix.)
		//
		// The example above is slightly special to go list
		// because it's in the std module.  Otherwise,
		// vendored modules are simply modules whose directory
		// is vendor/ instead of GOMODCACHE, and the
		// import path equals the package path.
		//
		// But in GOPATH (non-module) mode, it's possible for
		// package vendoring to cause a non-identity ImportMap,
		// as in this example:
		//
		// $ cd $HOME/src
		// $ find . -type f
		// ./b/b.go
		// ./vendor/example.com/a/a.go
		// $ cat ./b/b.go
		// package b
		// import _ "example.com/a"
		// $ cat ./vendor/example.com/a/a.go
		// package a
		// $ GOPATH=$HOME GO111MODULE=off go list -json ./b | grep -A2 ImportMap
		//     "ImportMap": {
		//         "example.com/a": "vendor/example.com/a"
		//     },

		// Don't remember any imports with significant errors.
		//
		// The len=0 condition is a heuristic check for imports of
		// non-existent packages (for which go/packages will create
		// an edge to a synthesized node). The heuristic is unsound
		// because some valid packages have zero files, for example,
		// a directory containing only the file p_test.go defines an
		// empty package p.
		// TODO(adonovan): clarify this. Perhaps go/packages should
		// report which nodes were synthesized.
		if importPath != "unsafe" && len(imported.CompiledGoFiles) == 0 {
			depsByImpPath[importPath] = "" // missing
			continue
		}

		// Don't record self-import edges.
		// (This simplifies metadataGraph's cycle check.)
		if PackageID(imported.ID) == id {
			if len(pkg.Errors) == 0 {
				bug.Reportf("self-import without error in package %s", id)
			}
			continue
		}

		buildMetadata(updates, imported, loadDir, false) // only top level packages can be standalone

		// Don't record edges to packages with no name, as they cause trouble for
		// the importer (golang/go#60952).
		//
		// However, we do want to insert these packages into the update map
		// (buildMetadata above), so that we get type-checking diagnostics for the
		// invalid packages.
		if imported.Name == "" {
			depsByImpPath[importPath] = "" // missing
			continue
		}

		depsByImpPath[importPath] = PackageID(imported.ID)
		depsByPkgPath[PackagePath(imported.PkgPath)] = PackageID(imported.ID)
	}
	mp.DepsByImpPath = depsByImpPath
	mp.DepsByPkgPath = depsByPkgPath

	// m.Diagnostics is set later in the loading pass, using
	// computeLoadDiagnostics.
}

// isWorkspacePackageLocked reports whether p is a workspace package for the
// snapshot s.
//
// Workspace packages are packages that we consider the user to be actively
// working on. As such, they are re-diagnosed on every keystroke, and searched
// for various workspace-wide queries such as references or workspace symbols.
//
// See the commentary inline for a description of the workspace package
// heuristics.
//
// s.mu must be held while calling this function.
func isWorkspacePackageLocked(s *Snapshot, meta *metadata.Graph, pkg *metadata.Package) bool {
	if metadata.IsCommandLineArguments(pkg.ID) {
		// Ad-hoc command-line-arguments packages aren't workspace packages.
		// With zero-config gopls (golang/go#57979) they should be very rare, as
		// they should only arise when the user opens a file outside the workspace
		// which isn't present in the import graph of a workspace package.
		//
		// Considering them as workspace packages tends to be racy, as they don't
		// deterministically belong to any view.
		if !pkg.Standalone {
			return false
		}

		// If all the files contained in pkg have a real package, we don't need to
		// keep pkg as a workspace package.
		if allFilesHaveRealPackages(meta, pkg) {
			return false
		}

		// For now, allow open standalone packages (i.e. go:build ignore) to be
		// workspace packages, but this means they could belong to multiple views.
		return containsOpenFileLocked(s, pkg)
	}

	// Apply filtering logic.
	//
	// Workspace packages must contain at least one non-filtered file.
	filterFunc := s.view.filterFunc()
	uris := make(map[protocol.DocumentURI]unit) // filtered package URIs
	for _, uri := range slices.Concat(pkg.CompiledGoFiles, pkg.GoFiles) {
		if !strings.Contains(string(uri), "/vendor/") && !filterFunc(uri) {
			uris[uri] = struct{}{}
		}
	}
	if len(uris) == 0 {
		return false // no non-filtered files
	}

	// For non-module views (of type GOPATH or AdHoc), or if
	// expandWorkspaceToModule is unset, workspace packages must be contained in
	// the workspace folder.
	//
	// For module views (of type GoMod or GoWork), packages must in any case be
	// in a workspace module (enforced below).
	if !s.view.moduleMode() || !s.Options().ExpandWorkspaceToModule {
		folder := s.view.folder.Dir.Path()
		inFolder := false
		for uri := range uris {
			if pathutil.InDir(folder, uri.Path()) {
				inFolder = true
				break
			}
		}
		if !inFolder {
			return false
		}
	}

	// In module mode, a workspace package must be contained in a workspace
	// module.
	if s.view.moduleMode() {
		if pkg.Module == nil {
			return false
		}
		modURI := protocol.URIFromPath(pkg.Module.GoMod)
		_, ok := s.view.workspaceModFiles[modURI]
		return ok
	}

	return true // an ad-hoc package or GOPATH package
}

// containsOpenFileLocked reports whether any file referenced by m is open in
// the snapshot s.
//
// s.mu must be held while calling this function.
func containsOpenFileLocked(s *Snapshot, mp *metadata.Package) bool {
	uris := map[protocol.DocumentURI]struct{}{}
	for _, uri := range mp.CompiledGoFiles {
		uris[uri] = struct{}{}
	}
	for _, uri := range mp.GoFiles {
		uris[uri] = struct{}{}
	}

	for uri := range uris {
		fh, _ := s.files.get(uri)
		if _, open := fh.(*overlay); open {
			return true
		}
	}
	return false
}

// computeWorkspacePackagesLocked computes workspace packages in the
// snapshot s for the given metadata graph. The result does not
// contain intermediate test variants.
//
// s.mu must be held while calling this function.
func computeWorkspacePackagesLocked(s *Snapshot, meta *metadata.Graph) immutable.Map[PackageID, PackagePath] {
	workspacePackages := make(map[PackageID]PackagePath)
	for _, mp := range meta.Packages {
		if !isWorkspacePackageLocked(s, meta, mp) {
			continue
		}

		switch {
		case mp.ForTest == "":
			// A normal package.
			workspacePackages[mp.ID] = mp.PkgPath
		case mp.ForTest == mp.PkgPath, mp.ForTest+"_test" == mp.PkgPath:
			// The test variant of some workspace package or its x_test.
			// To load it, we need to load the non-test variant with -test.
			//
			// Notably, this excludes intermediate test variants from workspace
			// packages.
			assert(!mp.IsIntermediateTestVariant(), "unexpected ITV")
			workspacePackages[mp.ID] = mp.ForTest
		}
	}
	return immutable.MapOf(workspacePackages)
}

// allFilesHaveRealPackages reports whether all files referenced by m are
// contained in a "real" package (not command-line-arguments).
//
// If m is valid but all "real" packages containing any file are invalid, this
// function returns false.
//
// If m is not a command-line-arguments package, this is trivially true.
func allFilesHaveRealPackages(g *metadata.Graph, mp *metadata.Package) bool {
	n := len(mp.CompiledGoFiles)
checkURIs:
	for _, uri := range append(mp.CompiledGoFiles[0:n:n], mp.GoFiles...) {
		for _, id := range g.IDs[uri] {
			if !metadata.IsCommandLineArguments(id) {
				continue checkURIs
			}
		}
		return false
	}
	return true
}

func isTestMain(pkg *packages.Package, gocache string) bool {
	// Test mains must have an import path that ends with ".test".
	if !strings.HasSuffix(pkg.PkgPath, ".test") {
		return false
	}
	// Test main packages are always named "main".
	if pkg.Name != "main" {
		return false
	}
	// Test mains always have exactly one GoFile that is in the build cache.
	if len(pkg.GoFiles) > 1 {
		return false
	}
	if !pathutil.InDir(gocache, pkg.GoFiles[0]) {
		return false
	}
	return true
}
