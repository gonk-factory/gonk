package gitexport

import (
	"fmt"
	"strings"
)

// RecordOptions are the inputs of `record`.
type RecordOptions struct {
	Version string
	// Parent, Root and Mirror are what Commit takes: the same snapshot is
	// rebuilt here, from origin, and nothing from an earlier run is trusted.
	Parent string
	Root   bool
	Mirror string
	// Confirm must equal the id of the snapshot commit. Without it Record
	// does everything except push, and says what it would push.
	Confirm string
	Check   CheckOptions
}

// RecordResult is the report of `record`.
type RecordResult struct {
	*CommitResult
	Origin string `json:"origin"`
	Ref    string `json:"ref"`
	Object string `json:"object"`
	Pushed bool   `json:"pushed"`
	// Already is true when origin already had the ref at this commit and
	// nothing was pushed.
	Already bool `json:"already_recorded"`
}

// ExportRef is where a snapshot commit is recorded on origin.
func ExportRef(version string) string { return "refs/export/" + version }

// Record pushes the snapshot commit to ORIGIN -- the internal repository the
// clone was made from -- as refs/export/<version>, so that the object the
// release review reads is addressable on the internal side before anything
// is public. Its ancestry is public history plus the snapshot (for a root:
// the snapshot alone), so it carries nothing origin does not already hold in
// another shape.
//
// It is the only function besides Publish that writes to a remote, and it
// cannot write to a mirror:
//
//  1. the target is the clone's own origin, and an origin on the public
//     host is refused before any work;
//  2. the snapshot is rebuilt and passes everything Commit demands;
//  3. the ref is refs/export/<version> and nothing else;
//  4. an export ref is never moved: origin having it at another commit is a
//     refusal; having it at this commit is reported and nothing is pushed;
//  5. Confirm names the exact commit id; one push, no force.
func (r *Repo) Record(src string, opt RecordOptions) (*RecordResult, error) {
	if isPublicHost(r.origin) {
		return nil, refuse("record pushes to the internal origin only; %s is the public host, and publishing there is publish's job", r.origin)
	}
	cr, err := r.Commit(src, CommitOptions{Version: opt.Version, Parent: opt.Parent, Root: opt.Root, Mirror: opt.Mirror, Check: opt.Check})
	res := &RecordResult{CommitResult: cr, Origin: r.origin, Ref: ExportRef(opt.Version)}
	if err != nil {
		return res, err
	}
	res.Object = cr.Commit

	existing, err := r.lsRemote(r.origin, res.Ref)
	if err != nil {
		return res, err
	}
	if existing == res.Object {
		res.Already = true
		return res, nil
	}
	if existing != "" {
		return res, refuse("origin already has %s at %s, and this snapshot is %s; an export ref is never moved", res.Ref, existing, res.Object)
	}
	if opt.Confirm != res.Object {
		return res, &NotConfirmed{Object: res.Object, Target: res.Ref, Mirror: r.origin}
	}

	// The one push to origin. No force flag: a ref that appeared between the
	// check above and now makes this fail.
	if _, err := netGit(r.dir, "push", "--quiet", "--", r.origin, res.Object+":"+res.Ref); err != nil {
		return res, err
	}
	after, err := r.lsRemote(r.origin, res.Ref)
	if err != nil {
		return res, err
	}
	if after != res.Object {
		return res, fmt.Errorf("after the push origin's %s is %s, not %s", res.Ref, orNone(after), res.Object)
	}
	res.Pushed = true
	return res, nil
}

// publicHost is the host of the public mirror. Record never pushes there.
const publicHost = "github.com"

// isPublicHost reports whether a git remote address is on the public host:
// scheme://[user@]host[:port]/path or the scp-like [user@]host:path.
func isPublicHost(remote string) bool {
	rest := remote
	if _, after, ok := strings.Cut(remote, "://"); ok {
		rest = after
		rest, _, _ = strings.Cut(rest, "/")
	} else {
		// scp-like, or a local path: a local path's first segment is "", "."
		// or a directory name, none of which is the host.
		rest, _, _ = strings.Cut(rest, ":")
		rest, _, _ = strings.Cut(rest, "/")
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	host, _, _ := strings.Cut(rest, ":")
	host = strings.ToLower(host)
	return host == publicHost || strings.HasSuffix(host, "."+publicHost)
}
