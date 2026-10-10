package basefacts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gonk-factory/gonk/pkg/glab"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fakeRepo is an in-memory Reader: entries by full path. Dirs are derived.
type fakeRepo struct {
	entries  map[string]glab.TreeEntry // full path -> entry
	blobs    map[string][]byte
	treeErr  map[string]error // dir -> error from ListTree
	blobErr  map[string]error // path -> error from GetRawFile
	rawCalls []string
}

func newRepo() *fakeRepo {
	return &fakeRepo{entries: map[string]glab.TreeEntry{}, blobs: map[string][]byte{}, treeErr: map[string]error{}, blobErr: map[string]error{}}
}

func (f *fakeRepo) add(path, typ, mode, content string) {
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		d := strings.Join(parts[:i], "/")
		if _, ok := f.entries[d]; !ok {
			f.entries[d] = glab.TreeEntry{Name: parts[i-1], Type: "tree", Mode: glab.ModeTree, Path: d}
		}
	}
	f.entries[path] = glab.TreeEntry{Name: parts[len(parts)-1], Type: typ, Mode: mode, Path: path}
	if typ == "blob" {
		f.blobs[path] = []byte(content)
	}
}

func (f *fakeRepo) ListTree(_ context.Context, _ int64, _, dir string) ([]glab.TreeEntry, error) {
	if err := f.treeErr[dir]; err != nil {
		return nil, err
	}
	if dir != "" {
		if e, ok := f.entries[dir]; !ok || !e.IsDir() {
			return nil, &glab.APIError{Status: 404, Method: "GET", Path: "/tree", Body: "404 Tree Not Found"}
		}
	}
	var out []glab.TreeEntry
	for p, e := range f.entries {
		parent := ""
		if i := strings.LastIndex(p, "/"); i >= 0 {
			parent = p[:i]
		}
		if parent == dir {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetRawFile(_ context.Context, _ int64, path, _ string, _ int64) ([]byte, error) {
	f.rawCalls = append(f.rawCalls, path)
	if err := f.blobErr[path]; err != nil {
		return nil, err
	}
	b, ok := f.blobs[path]
	if !ok {
		return nil, &glab.APIError{Status: 404, Method: "GET", Path: path, Body: "404 File Not Found"}
	}
	return b, nil
}

func repo() *fakeRepo {
	r := newRepo()
	r.add("README.md", "blob", "100644", "hello\n")
	r.add("bin/run.sh", "blob", "100755", "#!/bin/sh\n")
	r.add("docs/link.md", "blob", "120000", "../README.md")
	r.add("vendor/lib", "commit", "160000", "")
	r.add("pkg/a/a.go", "blob", "100644", "package a\n")
	r.add("evil", "blob", "120000", "pkg")
	return r
}

func TestReadEditPaths(t *testing.T) {
	rows := []struct {
		name       string
		edit       []string
		wantBlob   []string
		wantAbsent []string
		wantRefuse map[string]Reason
	}{
		{"regular file", []string{"README.md"}, []string{"README.md"}, nil, nil},
		{"executable file", []string{"bin/run.sh"}, []string{"bin/run.sh"}, nil, nil},
		{"symlink is refused, never editable", []string{"docs/link.md"}, nil, nil, map[string]Reason{"docs/link.md": ReasonSymlink}},
		{"top-level symlink", []string{"evil"}, nil, nil, map[string]Reason{"evil": ReasonSymlink}},
		{"submodule is refused", []string{"vendor/lib"}, nil, nil, map[string]Reason{"vendor/lib": ReasonSubmodule}},
		{"directory is refused", []string{"pkg"}, nil, nil, map[string]Reason{"pkg": ReasonDirectory}},
		{"missing file is absent", []string{"nope.txt"}, nil, []string{"nope.txt"}, nil},
		{"file in a missing dir is absent", []string{"nodir/x.go"}, nil, []string{"nodir/x.go"}, nil},
		{"path through a symlinked parent is refused", []string{"evil/a/a.go"}, nil, nil, map[string]Reason{"evil/a/a.go": ReasonParent}},
		{"path through a file parent is refused", []string{"README.md/x"}, nil, nil, map[string]Reason{"README.md/x": ReasonParent}},
		{"mixed", []string{"README.md", "docs/link.md", "nope.txt", "pkg/a/a.go"},
			[]string{"README.md", "pkg/a/a.go"}, []string{"nope.txt"}, map[string]Reason{"docs/link.md": ReasonSymlink}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rp := repo()
			f, err := Read(context.Background(), rp, 75, sha, r.edit, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Blobs) != len(r.wantBlob) {
				t.Fatalf("Blobs = %v, want %v", keys(f.Blobs), r.wantBlob)
			}
			for _, p := range r.wantBlob {
				if string(f.Blobs[p]) != string(rp.blobs[p]) {
					t.Errorf("Blobs[%s] = %q, want %q", p, f.Blobs[p], rp.blobs[p])
				}
			}
			if len(f.Absent) != len(r.wantAbsent) {
				t.Fatalf("Absent = %v, want %v", f.Absent, r.wantAbsent)
			}
			for _, p := range r.wantAbsent {
				if !f.Absent[p] {
					t.Errorf("%s not Absent", p)
				}
			}
			if len(f.Refusals) != len(r.wantRefuse) {
				t.Fatalf("Refusals = %v, want %v", f.Refusals, r.wantRefuse)
			}
			for _, rf := range f.Refusals {
				if r.wantRefuse[rf.Path] != rf.Reason {
					t.Errorf("%s refused as %q, want %q", rf.Path, rf.Reason, r.wantRefuse[rf.Path])
				}
				if _, leaked := f.Blobs[rf.Path]; leaked {
					t.Errorf("refused path %s also returned as a blob", rf.Path)
				}
			}
			// A refused or absent path's blob must never even be fetched.
			for _, p := range rp.rawCalls {
				if _, ok := f.Blobs[p]; !ok {
					t.Errorf("blob %s was fetched but not returned", p)
				}
			}
		})
	}
}

func TestReadCreatePaths(t *testing.T) {
	rows := []struct {
		name       string
		create     []string
		wantExists map[string]string // path -> Mode
		wantRefuse map[string]Reason
	}{
		{"new file in new dir", []string{"newdir/x_test.go"}, nil, nil},
		{"new file in existing dir", []string{"pkg/a/b.go"}, nil, nil},
		{"overwrite of a file", []string{"README.md"}, map[string]string{"README.md": "100644"}, nil},
		{"overwrite of a symlink (any type exists)", []string{"evil"}, map[string]string{"evil": "120000"}, nil},
		{"a directory exists at the path", []string{"pkg"}, map[string]string{"pkg": "040000"}, nil},
		{"through a symlinked parent", []string{"evil/new.go"}, nil, map[string]Reason{"evil/new.go": ReasonParent}},
		{"through a submodule parent", []string{"vendor/lib/new.go"}, nil, map[string]Reason{"vendor/lib/new.go": ReasonParent}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f, err := Read(context.Background(), repo(), 75, sha, nil, r.create)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Exists) != len(r.wantExists) {
				t.Fatalf("Exists = %v, want %v", f.Exists, r.wantExists)
			}
			for p, m := range r.wantExists {
				if f.Exists[p].Mode != m {
					t.Errorf("Exists[%s].Mode = %q, want %q", p, f.Exists[p].Mode, m)
				}
			}
			if len(f.Refusals) != len(r.wantRefuse) {
				t.Fatalf("Refusals = %v, want %v", f.Refusals, r.wantRefuse)
			}
			for _, rf := range f.Refusals {
				if r.wantRefuse[rf.Path] != rf.Reason {
					t.Errorf("%s refused as %q, want %q", rf.Path, rf.Reason, r.wantRefuse[rf.Path])
				}
			}
		})
	}
}

// A read error is an error, never "absent": a missing-looking answer is OURS to
// distinguish.
func TestReadErrorIsNeverAbsent(t *testing.T) {
	boom := errors.New("connection reset")
	e500 := &glab.APIError{Status: 500, Method: "GET", Path: "/x", Body: "oops"}
	rows := []struct {
		name  string
		setup func(*fakeRepo)
		edit  []string
		crt   []string
	}{
		{"root tree fails", func(r *fakeRepo) { r.treeErr[""] = boom }, []string{"README.md"}, nil},
		{"root tree 404 (unknown sha)", func(r *fakeRepo) { r.treeErr[""] = &glab.APIError{Status: 404, Body: "404 Tree Not Found"} }, []string{"README.md"}, nil},
		{"subdir tree 500 on an edit", func(r *fakeRepo) { r.treeErr["pkg/a"] = e500 }, []string{"pkg/a/a.go"}, nil},
		{"subdir tree 500 on a create", func(r *fakeRepo) { r.treeErr["pkg/a"] = e500 }, nil, []string{"pkg/a/new.go"}},
		{"blob read fails", func(r *fakeRepo) { r.blobErr["README.md"] = boom }, []string{"README.md"}, nil},
		{"blob 404 after the tree listed it", func(r *fakeRepo) { delete(r.blobs, "README.md") }, []string{"README.md"}, nil},
		{"blob too large", func(r *fakeRepo) { r.blobErr["README.md"] = fmt.Errorf("x: %w", glab.ErrResponseTooLarge) }, []string{"README.md"}, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rp := repo()
			r.setup(rp)
			f, err := Read(context.Background(), rp, 75, sha, r.edit, r.crt)
			if err == nil || f != nil {
				t.Fatalf("got (%+v, %v): a read error must be an error and no Facts", f, err)
			}
		})
	}
}

func TestReadRejectsBadInput(t *testing.T) {
	for _, s := range []string{"main", "", sha[:10]} {
		if _, err := Read(context.Background(), repo(), 75, s, []string{"README.md"}, nil); err == nil {
			t.Errorf("base %q accepted", s)
		}
	}
	for _, p := range []string{"", "/abs", "a//b", "a/../b", "./a", "a/"} {
		if _, err := Read(context.Background(), repo(), 75, sha, []string{p}, nil); err == nil {
			t.Errorf("edit path %q accepted", p)
		}
		if _, err := Read(context.Background(), repo(), 75, sha, nil, []string{p}); err == nil {
			t.Errorf("create path %q accepted", p)
		}
	}
}

// Through the real glab client over HTTP: a symlink entry on page TWO of the
// root listing is still refused, and a 5xx on the tree is an error.
func TestReadThroughGlabClient(t *testing.T) {
	entry := func(name, typ, mode string) string {
		return fmt.Sprintf(`{"id":"i","name":%q,"type":%q,"path":%q,"mode":%q}`, name, typ, name, mode)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/75/repository/tree", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != sha {
			http.Error(w, `{"message":"404 Tree Not Found"}`, 404)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprint(w, "["+entry("link", "blob", "120000")+"]")
			return
		}
		w.Header().Set("X-Next-Page", "2")
		_, _ = fmt.Fprint(w, "["+entry("README.md", "blob", "100644")+"]")
	})
	mux.HandleFunc("/api/v4/projects/75/repository/files/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "hello")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := glab.New(srv.URL, "tok")
	f, err := Read(context.Background(), c, 75, sha, []string{"README.md", "link"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Blobs["README.md"]) != "hello" {
		t.Errorf("Blobs = %v", f.Blobs)
	}
	if len(f.Refusals) != 1 || f.Refusals[0].Path != "link" || f.Refusals[0].Reason != ReasonSymlink || f.Refusals[0].Entry.Mode != "120000" {
		t.Fatalf("Refusals = %+v: the symlink on page two must be refused", f.Refusals)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
