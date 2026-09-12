// Session transcript forensics (gonk-pop3, item 2).
//
// THE PROBLEM. gonk-sweep reads a finished session's transcript, classifies the
// work from it, and then the reaper destroys the agent pod. The transcript is
// then gone FOREVER -- it lives only in the supervisor's session, which the
// close tears down. On 2026-09-12 that meant a wrong triage answer could not be
// investigated at all: by the time anyone asked why, there was nothing left to
// read. One earlier investigation only succeeded because someone happened to
// `kubectl exec` into the pod inside the ten-minute reap window; the next one
// missed the window and the evidence was lost.
//
// TWO THINGS LIVE HERE, and the split is deliberate.
//
//  1. transcriptRecord -- ALWAYS ON. Metadata only: how many turns, how many
//     bytes, a content hash, whether the batch fence was present, whether the
//     read was paginated or empty. It carries NO transcript text. This is cheap
//     (one line per classified session), safe (model and user text never
//     reaches it), and on its own it already separates "the agent said nothing"
//     from "we could not read what it said" -- the ambiguity that cost issue
//     !42 a wrongful escalation.
//
//  2. archiveTranscript -- DEV ONLY, off unless GONK_TRANSCRIPT_DIR is set.
//     It writes the whole transcript, which is untrusted model output over
//     untrusted user input and can be megabytes. That is not something to spray
//     into a container log or to persist by default.
//
// RETENTION, and why an emptyDir. The archive lives on the CONTROLLER pod, not
// the agent pod. That is the entire point: it outlives the session and the
// reaper, which is where the evidence was being lost. It does NOT outlive a
// controller restart, and that is a deliberate limit -- a PVC would be durable
// storage of untrusted text with no retention policy and no access control,
// which is a much bigger commitment than "let me see what the agent said this
// afternoon". The chart mounts it with a sizeLimit and this file prunes by
// count, so a busy afternoon cannot fill the node.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitlab.orac.local/agentic/gonk-project/pkg/beadstore"
	"gitlab.orac.local/agentic/gonk-project/pkg/gcapi"
)

// transcriptDirEnv names the dev-only archive directory. UNSET OR EMPTY MEANS
// OFF, and that is the production default: the switch has to be asked for by
// name, which is what "dev-only" means here.
const transcriptDirEnv = "GONK_TRANSCRIPT_DIR"

// maxArchivedTranscripts bounds the directory. The sweep runs every 30s and a
// busy afternoon can classify a lot of sessions; an unbounded archive on an
// emptyDir is a node-eviction waiting to happen (this namespace has already
// lost pods that way).
const maxArchivedTranscripts = 200

// maxArchivedTranscriptBytes bounds a single file. gcapi refuses to read a
// transcript over 4 MiB at all, so this cannot truncate a transcript the sweep
// actually judged; it is a second belt on the same trousers.
const maxArchivedTranscriptBytes = 4 << 20

// transcriptRecord is the always-on metadata line. Every field is a count, a
// hash or a boolean -- deliberately nothing that could carry a prompt, a
// comment body, a model answer or a credential.
type transcriptRecord struct {
	Bead      string
	Session   string
	Turns     int
	Roles     string // e.g. "assistant=4,user=1" -- the shape of the conversation
	Bytes     int
	SHA256    string
	Fence     bool
	Paginated bool
	Empty     bool
	Archived  string // the archive path, or "" when the archive is off
}

// describeTranscript builds the record from a transcript that has been read.
// TURN COUNT IS THE POINT: learning that one run made 6 model calls and another
// 9 previously required a port-forward to gonk-meter and a bearer token against
// /v1/cost/bead. The turn shape is free, comes from a process that survives the
// pod, and answers most of the same question.
func describeTranscript(rec beadstore.Record, tr *gcapi.SessionTranscript) transcriptRecord {
	text := tr.Text()
	sum := sha256.Sum256([]byte(text))
	out := transcriptRecord{
		Bead:    rec.BeadAnchor,
		Session: rec.SessionID,
		Turns:   len(tr.Turns),
		Roles:   roleHistogram(tr),
		Bytes:   len(text),
		SHA256:  hex.EncodeToString(sum[:])[:16],
		Fence:   strings.Contains(text, batchStartSentinel),
		Empty:   isUnreadableTranscript(text),
	}
	if tr.Pagination != nil {
		out.Paginated = tr.Pagination.HasMore
	}
	return out
}

func roleHistogram(tr *gcapi.SessionTranscript) string {
	counts := map[string]int{}
	for _, t := range tr.Turns {
		role := t.Role
		if role == "" {
			role = "unknown"
		}
		counts[role]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", sanitizeRole(k), counts[k]))
	}
	return strings.Join(parts, ",")
}

// sanitizeRole keeps an upstream-supplied role name from putting control bytes
// or separators into a log field.
func sanitizeRole(s string) string {
	if len(s) > 24 {
		s = s[:24]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('.')
		}
	}
	return b.String()
}

func (r transcriptRecord) log(log *slog.Logger) {
	attrs := []any{
		"bead", r.Bead, "session", r.Session,
		"turns", r.Turns, "roles", r.Roles,
		"bytes", r.Bytes, "sha256", r.SHA256,
		"batch_fence", r.Fence,
	}
	if r.Paginated {
		attrs = append(attrs, "paginated", true)
	}
	if r.Archived != "" {
		attrs = append(attrs, "archived", r.Archived)
	}
	switch {
	case r.Empty:
		// An empty transcript is a FAILED READ, not a silent agent (gonk-2tb),
		// and it is the single most expensive thing that can go wrong here: it
		// burns an attempt and re-slings onto a pricier rung with the agent's
		// completed work sitting unread. It gets an ERROR.
		log.Error("transcript read EMPTY: the session cannot be judged from it", attrs...)
	case r.Paginated:
		log.Error("transcript read PARTIAL: refusing to judge a paginated read", attrs...)
	default:
		log.Info("transcript read", attrs...)
	}
}

// archiveTranscript writes the transcript under dir and returns the path it
// wrote, or "" when the archive is off or the write failed.
//
// IT NEVER FAILS THE SWEEP. Losing an archive copy is a smaller problem than a
// classification pass that aborts -- and a sweep that died on a full disk would
// reproduce gonk-p7qh's failure (a dead outcome path) in a new costume.
func archiveTranscript(log *slog.Logger, dir string, rec beadstore.Record, tr *gcapi.SessionTranscript) string {
	if dir == "" {
		return "" // the default: dev-only, off unless asked for by name
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warn("transcript archive: could not create the directory; keeping only the metadata record",
			"dir", dir, "err", err)
		return ""
	}

	body, err := json.MarshalIndent(struct {
		BeadAnchor string                   `json:"bead_anchor"`
		SessionID  string                   `json:"session_id"`
		Project    string                   `json:"project"`
		IssueIID   int64                    `json:"issue_iid"`
		Trigger    string                   `json:"trigger"`
		Attempt    int                      `json:"attempt"`
		Rung       string                   `json:"rung"`
		Transcript *gcapi.SessionTranscript `json:"transcript"`
	}{
		BeadAnchor: rec.BeadAnchor, SessionID: rec.SessionID, Project: rec.Project,
		IssueIID: rec.IssueIID, Trigger: rec.Trigger, Attempt: rec.Attempt, Rung: rec.Rung,
		Transcript: tr,
	}, "", "  ")
	if err != nil {
		log.Warn("transcript archive: could not encode the transcript", "bead", rec.BeadAnchor, "err", err)
		return ""
	}
	if len(body) > maxArchivedTranscriptBytes {
		log.Warn("transcript archive: transcript is over the per-file cap; not archiving",
			"bead", rec.BeadAnchor, "bytes", len(body), "cap", maxArchivedTranscriptBytes)
		return ""
	}

	name := transcriptFileName(rec)
	path := filepath.Join(dir, name)
	// Atomic: a reader must never see a half-written transcript, and a sweep
	// killed mid-write must not leave one behind.
	tmp, err := os.CreateTemp(dir, ".tmp-"+name+"-")
	if err != nil {
		log.Warn("transcript archive: could not open a temp file", "dir", dir, "err", err)
		return ""
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		log.Warn("transcript archive: write failed", "bead", rec.BeadAnchor, "err", err)
		return ""
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		log.Warn("transcript archive: close failed", "bead", rec.BeadAnchor, "err", err)
		return ""
	}
	// 0600, not 0644: this is untrusted user and model text, and the pod it
	// lands in is shared with the supervisor.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		log.Warn("transcript archive: chmod failed", "path", tmpName, "err", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		log.Warn("transcript archive: rename failed", "path", path, "err", err)
		return ""
	}

	pruneTranscripts(log, dir, maxArchivedTranscripts)
	return path
}

// transcriptFileName is deterministic and filesystem-safe. A bead anchor
// carries colons (`gonk:75:issue:71`) and a session alias is upstream-supplied,
// so both are slugged -- and the result is then bounded, because a path that
// exceeds NAME_MAX turns an archive write into a confusing errno.
func transcriptFileName(rec beadstore.Record) string {
	base := slugForPath(rec.BeadAnchor) + "--" + slugForPath(rec.SessionID)
	if len(base) > 180 {
		base = base[:180]
	}
	return base + ".json"
}

// slugForPath maps anything to [a-zA-Z0-9._-]. It also refuses to produce "."
// or ".." or an empty string, so a crafted alias cannot escape the directory
// or name the directory itself.
func slugForPath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "unnamed"
	}
	return out
}

// pruneTranscripts keeps the newest `keep` files and deletes the rest. Oldest
// first, by modification time.
func pruneTranscripts(log *slog.Logger, dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type f struct {
		name string
		mod  int64
	}
	var files []f
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, f{e.Name(), info.ModTime().UnixNano()})
	}
	if len(files) <= keep {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].mod != files[j].mod {
			return files[i].mod < files[j].mod
		}
		return files[i].name < files[j].name
	})
	for _, old := range files[:len(files)-keep] {
		if err := os.Remove(filepath.Join(dir, old.name)); err != nil {
			log.Warn("transcript archive: prune failed", "file", old.name, "err", err)
		}
	}
}
