// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/gocommand"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/lsp/protocol"
	"github.com/govim/govim/cmd/govim/internal/golang_org_x_tools/span"
)

func GCOptimizationDetails(ctx context.Context, snapshot Snapshot, pkgDir span.URI) (map[VersionedFileIdentity][]*Diagnostic, error) {
	outDir := filepath.Join(os.TempDir(), fmt.Sprintf("gopls-%d.details", os.Getpid()))

	if err := os.MkdirAll(outDir, 0700); err != nil {
		return nil, err
	}
	tmpFile, err := ioutil.TempFile(os.TempDir(), "gopls-x")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name())

	outDirURI := span.URIFromPath(outDir)
	// GC details doesn't handle Windows URIs in the form of "file:///C:/...",
	// so rewrite them to "file://C:/...". See golang/go#41614.
	if !strings.HasPrefix(outDir, "/") {
		outDirURI = span.URI(strings.Replace(string(outDirURI), "file:///", "file://", 1))
	}
	inv := &gocommand.Invocation{
		Verb: "build",
		Args: []string{
			fmt.Sprintf("-gcflags=-json=0,%s", outDirURI),
			fmt.Sprintf("-o=%s", tmpFile.Name()),
			".",
		},
		WorkingDir: pkgDir.Filename(),
	}
	_, err = snapshot.RunGoCommandDirect(ctx, Normal, inv)
	if err != nil {
		return nil, err
	}
	files, err := findJSONFiles(outDir)
	if err != nil {
		return nil, err
	}
	reports := make(map[VersionedFileIdentity][]*Diagnostic)
	opts := snapshot.View().Options()
	var parseError error
	for _, fn := range files {
		uri, diagnostics, err := parseDetailsFile(fn, opts)
		if err != nil {
			// expect errors for all the files, save 1
			parseError = err
		}
		fh := snapshot.FindFile(uri)
		if fh == nil {
			continue
		}
		if pkgDir.Filename() != filepath.Dir(fh.URI().Filename()) {
			// https://github.com/golang/go/issues/42198
			// sometimes the detail diagnostics generated for files
			// outside the package can never be taken back.
			continue
		}
		reports[fh.VersionedFileIdentity()] = diagnostics
	}
	return reports, parseError
}

func parseDetailsFile(filename string, options *Options) (span.URI, []*Diagnostic, error) {
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", nil, err
	}
	var (
		uri         span.URI
		i           int
		diagnostics []*Diagnostic
	)
	type metadata struct {
		File string `json:"file,omitempty"`
	}
	for dec := json.NewDecoder(bytes.NewReader(buf)); dec.More(); {
		// The first element always contains metadata.
		if i == 0 {
			i++
			m := new(metadata)
			if err := dec.Decode(m); err != nil {
				return "", nil, err
			}
			if !strings.HasSuffix(m.File, ".go") {
				continue // <autogenerated>
			}
			uri = span.URIFromPath(m.File)
			continue
		}
		d := new(protocol.Diagnostic)
		if err := dec.Decode(d); err != nil {
			return "", nil, err
		}
		msg := d.Code.(string)
		if msg != "" {
			msg = fmt.Sprintf("%s(%s)", msg, d.Message)
		}
		if skipDiagnostic(msg, d.Source, options) {
			continue
		}
		var related []RelatedInformation
		for _, ri := range d.RelatedInformation {
			related = append(related, RelatedInformation{
				URI:     ri.Location.URI.SpanURI(),
				Range:   zeroIndexedRange(ri.Location.Range),
				Message: ri.Message,
			})
		}
		diagnostic := &Diagnostic{
			Range:    zeroIndexedRange(d.Range),
			Message:  msg,
			Severity: d.Severity,
			Source:   d.Source,
			Tags:     d.Tags,
			Related:  related,
		}
		diagnostics = append(diagnostics, diagnostic)
		i++
	}
	return uri, diagnostics, nil
}

// skipDiagnostic reports whether a given diagnostic should be shown to the end
// user, given the current options.
func skipDiagnostic(msg, source string, o *Options) bool {
	if source != "go compiler" {
		return false
	}
	switch {
	case o.Annotations["noInline"]:
		return strings.HasPrefix(msg, "canInline") ||
			strings.HasPrefix(msg, "cannotInline") ||
			strings.HasPrefix(msg, "inlineCall")
	case o.Annotations["noEscape"]:
		return strings.HasPrefix(msg, "escape") || msg == "leak"
	case o.Annotations["noNilcheck"]:
		return strings.HasPrefix(msg, "nilcheck")
	case o.Annotations["noBounds"]:
		return strings.HasPrefix(msg, "isInBounds") ||
			strings.HasPrefix(msg, "isSliceInBounds")
	}
	return false
}

// The range produced by the compiler is 1-indexed, so subtract range by 1.
func zeroIndexedRange(rng protocol.Range) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{
			Line:      rng.Start.Line - 1,
			Character: rng.Start.Character - 1,
		},
		End: protocol.Position{
			Line:      rng.End.Line - 1,
			Character: rng.End.Character - 1,
		},
	}
}

func findJSONFiles(dir string) ([]string, error) {
	ans := []string{}
	f := func(path string, fi os.FileInfo, _ error) error {
		if fi.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".json") {
			ans = append(ans, path)
		}
		return nil
	}
	err := filepath.Walk(dir, f)
	return ans, err
}