package main

import (
	"context"
	"fmt"

	"github.com/gonk-factory/gonk/pkg/glab"
	"github.com/gonk-factory/gonk/pkg/scope"
)

// rawFileGetter reads one file at one commit.
type rawFileGetter interface {
	GetRawFile(ctx context.Context, projectID int64, path, ref string, maxBytes int64) ([]byte, error)
}

// pipefixCIIncludes resolves the local includes of the pipeline definition at
// one commit, through the files it includes and the files those include
// (scope.ResolveCILocalIncludes; gonk-9q0y). A project with no definition
// file has none, and that is complete.
//
// The two errors are told apart because the callers say different things:
// readErr is the root file that could not be read (GitLab's failure);
// includesErr is a definition that cannot be understood, or one of its
// included files that could not be read.
func pipefixCIIncludes(ctx context.Context, r rawFileGetter, projectID int64, sha string, maxBytes int64) (inc scope.CIIncludes, readErr, includesErr error) {
	root, err := r.GetRawFile(ctx, projectID, pipefixCIFile, sha, maxBytes)
	switch {
	case glab.IsNotFound(err): // includes:no-definition
		return scope.CIIncludes{Complete: true}, nil, nil
	case err != nil:
		return scope.CIIncludes{}, err, nil
	}
	inc, err = scope.ResolveCILocalIncludes(root, func(path string) ([]byte, bool, error) {
		b, ferr := r.GetRawFile(ctx, projectID, path, sha, maxBytes)
		if glab.IsNotFound(ferr) {
			return nil, false, nil
		}
		return b, ferr == nil, ferr
	})
	if err != nil {
		return scope.CIIncludes{}, nil, fmt.Errorf("%s at %s: %w", pipefixCIFile, sha, err)
	}
	return inc, nil, nil
}
